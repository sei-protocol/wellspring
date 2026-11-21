package store

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Errors CreateGrant returns for a refused grant.
var (
	ErrGrantConflict = errors.New("grant ID already used for a different request")
	ErrGrantNoFunds  = errors.New("grant would take the faucet below its minimum balance")
)

// Grant is an operator's transfer to one address. Callers name their grants,
// so a retried request finds the grant it already made instead of paying again.
type Grant struct {
	Caller  string
	ID      string
	Address common.Address
	// Request is what the caller asked for; a retry must ask for the same.
	Request string
	// Amount is what is sent: zero when the recipient already held enough.
	Amount    *big.Int
	Reason    string
	CreatedAt time.Time
	// Claim carries the transfer; nil when Amount is zero.
	Claim *Claim
}

// CreateGrant stores g and queues its transfer, unless the caller already has
// a grant with g's ID, which it returns instead with created false. A new grant
// is refused if it, together with every unsettled claim, would spend more than
// available.
func (s *Store) CreateGrant(ctx context.Context, g *Grant, available *big.Int, now time.Time) (*Grant, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := getGrant(ctx, tx, g.Caller, g.ID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if existing.Address != g.Address || existing.Request != g.Request {
			return nil, false, ErrGrantConflict
		}
		return existing, false, nil
	}

	out := *g
	out.Amount = new(big.Int).Set(g.Amount)
	out.CreatedAt = time.UnixMilli(now.UnixMilli())
	var claimID sql.NullString
	if out.Amount.Sign() > 0 {
		unsettled, err := unsettledWei(ctx, tx)
		if err != nil {
			return nil, false, err
		}
		if unsettled.Add(unsettled, out.Amount).Cmp(available) > 0 {
			return nil, false, ErrGrantNoFunds
		}
		out.Claim = &Claim{ID: newID(), Kind: KindGrant, Address: g.Address, Amount: out.Amount, Status: StatusQueued, CreatedAt: out.CreatedAt}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO claims (id, kind, address, ip, amount, status, created_at) VALUES (?, ?, ?, '', ?, ?, ?)`,
			out.Claim.ID, KindGrant, g.Address.Hex(), out.Amount.String(), StatusQueued, now.UnixMilli()); err != nil {
			return nil, false, err
		}
		claimID = sql.NullString{String: out.Claim.ID, Valid: true}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO grants (caller, id, address, request, amount, claim_id, reason, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		g.Caller, g.ID, g.Address.Hex(), g.Request, out.Amount.String(), claimID, g.Reason, now.UnixMilli()); err != nil {
		return nil, false, err
	}
	return &out, true, tx.Commit()
}

// GetGrant returns the caller's grant with id, or nil.
func (s *Store) GetGrant(ctx context.Context, caller, id string) (*Grant, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return getGrant(ctx, tx, caller, id)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func getGrant(ctx context.Context, db querier, caller, id string) (*Grant, error) {
	g := Grant{Caller: caller, ID: id}
	var address, amount string
	var claimID sql.NullString
	var created int64
	err := db.QueryRowContext(ctx,
		`SELECT address, request, amount, claim_id, reason, created_at FROM grants WHERE caller = ? AND id = ?`,
		caller, id).Scan(&address, &g.Request, &amount, &claimID, &g.Reason, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.Address = common.HexToAddress(address)
	g.Amount, _ = new(big.Int).SetString(amount, 10)
	g.CreatedAt = time.UnixMilli(created)
	if claimID.Valid {
		claims, err := queryClaims(ctx, db, `WHERE id = ?`, claimID.String)
		if err != nil {
			return nil, err
		}
		if len(claims) == 1 {
			g.Claim = claims[0]
		}
	}
	return &g, nil
}

// unsettledWei sums the amounts of every queued or sent claim: what the faucet
// has promised but its balance may not yet show.
func unsettledWei(ctx context.Context, db querier) (*big.Int, error) {
	rows, err := db.QueryContext(ctx, `SELECT amount FROM claims WHERE status IN (?, ?)`, StatusQueued, StatusSent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	total := new(big.Int)
	for rows.Next() {
		var amount string
		if err := rows.Scan(&amount); err != nil {
			return nil, err
		}
		v, ok := new(big.Int).SetString(amount, 10)
		if !ok {
			return nil, errors.New("claim amount is not a decimal integer")
		}
		total.Add(total, v)
	}
	return total, rows.Err()
}
