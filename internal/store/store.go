// Package store persists faucet claims and enforces their rate limits.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"math/big"
	"net/url"
	"time"

	"github.com/ethereum/go-ethereum/common"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Status is where a claim is in its lifecycle: queued, then sent once its
// transaction is signed, then confirmed or failed by its receipt.
type Status string

// Claim statuses.
const (
	StatusQueued    Status = "queued"
	StatusSent      Status = "sent"
	StatusConfirmed Status = "confirmed"
	StatusFailed    Status = "failed"
)

// Errors CreateClaim returns for a refused claim.
var (
	ErrAddressCooldown = errors.New("address claimed recently")
	ErrIPLimit         = errors.New("too many claims from this IP")
	ErrQueueFull       = errors.New("faucet queue is full")
)

// Claim is one request for funds.
type Claim struct {
	ID        string
	Address   common.Address
	IP        string
	Amount    *big.Int
	Status    Status
	Nonce     sql.NullInt64
	TxHash    string
	RawTx     []byte
	Error     string
	CreatedAt time.Time
	SentAt    sql.NullTime
	DoneAt    sql.NullTime
}

// Limits bounds who may claim. Failed claims count against none of them.
type Limits struct {
	AddressCooldown time.Duration
	IPWindow        time.Duration
	IPLimit         int
	MaxQueued       int
}

// Store is a SQLite-backed claim store. One process owns the file.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS claims (
	id         TEXT PRIMARY KEY,
	address    TEXT NOT NULL,
	ip         TEXT NOT NULL,
	amount     TEXT NOT NULL,
	status     TEXT NOT NULL,
	nonce      INTEGER,
	tx_hash    TEXT NOT NULL DEFAULT '',
	raw_tx     BLOB,
	error      TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	sent_at    INTEGER,
	done_at    INTEGER
);
CREATE INDEX IF NOT EXISTS claims_address ON claims(address, created_at);
CREATE INDEX IF NOT EXISTS claims_ip ON claims(ip, created_at);
CREATE INDEX IF NOT EXISTS claims_status ON claims(status, created_at);
`

// Open opens or creates the store at path.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := url.URL{Scheme: "file", Opaque: url.PathEscape(path), RawQuery: url.Values{
		"_pragma": {"journal_mode(WAL)", "busy_timeout(5000)", "synchronous(FULL)"},
	}.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	// One connection serialises every write, which makes the limit checks atomic.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the store.
func (s *Store) Close() error { return s.db.Close() }

// CreateClaim queues a claim unless a limit refuses it, in which case it also
// returns how long until the caller may retry.
func (s *Store) CreateClaim(ctx context.Context, address common.Address, ip string, amount *big.Int, limits Limits, now time.Time) (*Claim, time.Duration, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var last sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM claims WHERE address = ? AND status != ? AND created_at > ?`,
		address.Hex(), StatusFailed, now.Add(-limits.AddressCooldown).UnixMilli()).Scan(&last); err != nil {
		return nil, 0, err
	}
	if last.Valid {
		return nil, time.UnixMilli(last.Int64).Add(limits.AddressCooldown).Sub(now), ErrAddressCooldown
	}
	var fromIP int
	var oldest sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(created_at) FROM claims WHERE ip = ? AND status != ? AND created_at > ?`,
		ip, StatusFailed, now.Add(-limits.IPWindow).UnixMilli()).Scan(&fromIP, &oldest); err != nil {
		return nil, 0, err
	}
	if fromIP >= limits.IPLimit {
		return nil, time.UnixMilli(oldest.Int64).Add(limits.IPWindow).Sub(now), ErrIPLimit
	}
	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM claims WHERE status = ?`, StatusQueued).Scan(&queued); err != nil {
		return nil, 0, err
	}
	if queued >= limits.MaxQueued {
		return nil, time.Minute, ErrQueueFull
	}
	c := &Claim{ID: newID(), Address: address, IP: ip, Amount: new(big.Int).Set(amount), Status: StatusQueued, CreatedAt: time.UnixMilli(now.UnixMilli())}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO claims (id, address, ip, amount, status, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, address.Hex(), ip, amount.String(), c.Status, now.UnixMilli()); err != nil {
		return nil, 0, err
	}
	return c, 0, tx.Commit()
}

// Get returns the claim with id, or nil.
func (s *Store) Get(ctx context.Context, id string) (*Claim, error) {
	claims, err := s.query(ctx, `WHERE id = ?`, id)
	if err != nil || len(claims) == 0 {
		return nil, err
	}
	return claims[0], nil
}

// Queued returns up to limit queued claims, oldest first.
func (s *Store) Queued(ctx context.Context, limit int) ([]*Claim, error) {
	return s.query(ctx, `WHERE status = ? ORDER BY created_at, id LIMIT ?`, StatusQueued, limit)
}

// InFlight returns every sent claim, lowest nonce first.
func (s *Store) InFlight(ctx context.Context) ([]*Claim, error) {
	return s.query(ctx, `WHERE status = ? ORDER BY nonce`, StatusSent)
}

// CountQueued returns the number of queued claims.
func (s *Store) CountQueued(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM claims WHERE status = ?`, StatusQueued).Scan(&n)
	return n, err
}

// MarkSent records a queued claim's signed transaction. Call it before
// broadcasting, so a restart can reconcile the transaction.
func (s *Store) MarkSent(ctx context.Context, id string, nonce uint64, hash common.Hash, raw []byte, now time.Time) error {
	return s.update(ctx, StatusQueued, `status = ?, nonce = ?, tx_hash = ?, raw_tx = ?, sent_at = ?`,
		id, StatusSent, int64(nonce), hash.Hex(), raw, now.UnixMilli()) //nolint:gosec // nonces stay far below 2^63
}

// Requeue returns a sent claim whose transaction never landed to the queue.
func (s *Store) Requeue(ctx context.Context, id string) error {
	return s.update(ctx, StatusSent, `status = ?, nonce = NULL, tx_hash = '', raw_tx = NULL, sent_at = NULL`, id, StatusQueued)
}

// Finish records a sent claim's final status.
func (s *Store) Finish(ctx context.Context, id string, status Status, errMsg string, now time.Time) error {
	if status != StatusConfirmed && status != StatusFailed {
		return ErrWrongStatus
	}
	return s.update(ctx, StatusSent, `status = ?, error = ?, done_at = ?`, id, status, errMsg, now.UnixMilli())
}

// ErrWrongStatus is returned for a transition the claim's status does not allow.
var ErrWrongStatus = errors.New("claim is not in the expected status")

func (s *Store) update(ctx context.Context, from Status, set, id string, args ...any) error {
	res, err := s.db.ExecContext(ctx, `UPDATE claims SET `+set+` WHERE id = ? AND status = ?`, append(args, id, from)...) //nolint:gosec // set is a constant from this file
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return ErrWrongStatus
	}
	return nil
}

func (s *Store) query(ctx context.Context, where string, args ...any) ([]*Claim, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, address, ip, amount, status, nonce, tx_hash, raw_tx, error, created_at, sent_at, done_at FROM claims `+where, args...) //nolint:gosec // where is a constant from this file
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Claim
	for rows.Next() {
		var c Claim
		var address, amount string
		var created int64
		var sent, done sql.NullInt64
		if err := rows.Scan(&c.ID, &address, &c.IP, &amount, &c.Status, &c.Nonce, &c.TxHash, &c.RawTx, &c.Error, &created, &sent, &done); err != nil {
			return nil, err
		}
		c.Address = common.HexToAddress(address)
		c.Amount, _ = new(big.Int).SetString(amount, 10)
		c.CreatedAt = time.UnixMilli(created)
		c.SentAt = sql.NullTime{Time: time.UnixMilli(sent.Int64), Valid: sent.Valid}
		c.DoneAt = sql.NullTime{Time: time.UnixMilli(done.Int64), Valid: done.Valid}
		out = append(out, &c)
	}
	return out, rows.Err()
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
