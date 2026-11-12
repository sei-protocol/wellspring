// Package sender signs and broadcasts faucet transfers from a single key.
//
// Giga admits a sender's transaction only at exactly its next nonce, and
// eth_getTransactionCount reports only the committed nonce. So one goroutine
// owns the nonce and signs every transfer in order. A claim keeps the nonce it
// was signed at until its receipt settles it: after any doubt the sender
// rebroadcasts the same transactions rather than re-signing claims at new
// nonces, which could pay a claim twice if its first transaction still lands.
package sender

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/sei-protocol/wellspring/internal/store"
)

const (
	transferGas = 21_000
	rpcTimeout  = 10 * time.Second
)

// RPC is the subset of the Ethereum JSON-RPC client the sender uses.
type RPC interface {
	NonceAt(ctx context.Context, account common.Address, block *big.Int) (uint64, error)
	BalanceAt(ctx context.Context, account common.Address, block *big.Int) (*big.Int, error)
	SendTransaction(ctx context.Context, tx *types.Transaction) error
	TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error)
}

// Config configures a Sender.
type Config struct {
	ChainID        *big.Int
	Key            *ecdsa.PrivateKey
	GasTipCap      *big.Int
	GasFeeCap      *big.Int
	BatchSize      int
	Interval       time.Duration
	ConfirmTimeout time.Duration
}

type metrics struct {
	txsSent, sendErrors, resyncs, rpcErrors prometheus.Counter
	claimsDone                              *prometheus.CounterVec
	claimTime                               prometheus.Histogram
	balance, queueDepth, nextNonce          prometheus.Gauge
}

func newMetrics(reg prometheus.Registerer) *metrics {
	f := promauto.With(reg)
	return &metrics{
		txsSent:    f.NewCounter(prometheus.CounterOpts{Name: "wellspring_txs_sent_total", Help: "Transfers broadcast."}),
		sendErrors: f.NewCounter(prometheus.CounterOpts{Name: "wellspring_send_errors_total", Help: "Broadcasts and rebroadcasts the node refused."}),
		resyncs:    f.NewCounter(prometheus.CounterOpts{Name: "wellspring_nonce_resyncs_total", Help: "Nonce resynchronisations with the chain."}),
		rpcErrors:  f.NewCounter(prometheus.CounterOpts{Name: "wellspring_rpc_errors_total", Help: "Failed RPC reads."}),
		claimsDone: f.NewCounterVec(prometheus.CounterOpts{Name: "wellspring_claims_finished_total", Help: "Claims by final status."}, []string{"status"}),
		claimTime: f.NewHistogram(prometheus.HistogramOpts{
			Name: "wellspring_claim_seconds", Help: "Time from claim to confirmed transfer.", Buckets: prometheus.ExponentialBuckets(0.25, 2, 10),
		}),
		balance:    f.NewGauge(prometheus.GaugeOpts{Name: "wellspring_balance_wei", Help: "Faucet balance in wei."}),
		queueDepth: f.NewGauge(prometheus.GaugeOpts{Name: "wellspring_queue_depth", Help: "Claims waiting to be sent."}),
		nextNonce:  f.NewGauge(prometheus.GaugeOpts{Name: "wellspring_next_nonce", Help: "Nonce the next transfer is signed with."}),
	}
}

// Sender drains queued claims into transfers.
type Sender struct {
	cfg     Config
	rpc     timeoutRPC
	store   *store.Store
	from    common.Address
	signer  types.Signer
	metrics *metrics

	// nonce, needResync and resyncedAt are owned by Run's goroutine.
	nonce      uint64
	needResync bool
	resyncedAt time.Time

	mu          sync.Mutex // guards balance and balanceTime
	balance     *big.Int
	balanceTime time.Time
}

// New returns a Sender signing with cfg.Key and registering its metrics on reg.
func New(cfg Config, rpc RPC, st *store.Store, reg prometheus.Registerer) *Sender {
	return &Sender{
		cfg:        cfg,
		rpc:        timeoutRPC{rpc: rpc, timeout: rpcTimeout},
		store:      st,
		from:       crypto.PubkeyToAddress(cfg.Key.PublicKey),
		signer:     types.LatestSignerForChainID(cfg.ChainID),
		metrics:    newMetrics(reg),
		needResync: true,
	}
}

// Address returns the faucet address.
func (s *Sender) Address() common.Address { return s.from }

// Balance returns the last observed faucet balance and when it was observed.
func (s *Sender) Balance() (*big.Int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.balance, s.balanceTime
}

// Run sends queued claims until ctx ends.
func (s *Sender) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.step(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Sender) step(ctx context.Context) {
	if s.needResync {
		if err := s.resync(ctx); err != nil {
			s.metrics.rpcErrors.Inc()
			slog.Warn("nonce resync failed", "err", err)
			return
		}
	}
	if err := s.dispatch(ctx); err != nil {
		slog.Warn("dispatch failed", "err", err)
	}
	if err := s.confirm(ctx); err != nil {
		slog.Warn("confirm failed", "err", err)
	}
	if n, err := s.store.CountQueued(ctx); err == nil {
		s.metrics.queueDepth.Set(float64(n))
	}
}

// dispatch signs and broadcasts up to BatchSize queued claims, in nonce order.
func (s *Sender) dispatch(ctx context.Context) error {
	claims, err := s.store.Queued(ctx, s.cfg.BatchSize)
	if err != nil {
		return err
	}
	for _, c := range claims {
		tx, err := types.SignNewTx(s.cfg.Key, s.signer, &types.DynamicFeeTx{
			ChainID:   s.cfg.ChainID,
			Nonce:     s.nonce,
			GasTipCap: s.cfg.GasTipCap,
			GasFeeCap: s.cfg.GasFeeCap,
			Gas:       transferGas,
			To:        &c.Address,
			Value:     c.Amount,
		})
		if err != nil {
			return err
		}
		raw, err := tx.MarshalBinary()
		if err != nil {
			return err
		}
		// Persist before broadcasting so a crash cannot lose track of a sent nonce.
		if err := s.store.MarkSent(ctx, c.ID, s.nonce, tx.Hash(), raw, time.Now()); err != nil {
			return err
		}
		// The claim now holds this nonce whether or not the broadcast took, so
		// an ambiguous error cannot lead to a second transfer for it.
		s.nonce++
		s.metrics.nextNonce.Set(float64(s.nonce))
		if err := s.rpc.SendTransaction(ctx, tx); err != nil {
			s.metrics.sendErrors.Inc()
			s.needResync = true
			slog.Warn("broadcast failed", "claim", c.ID, "nonce", tx.Nonce(), "err", err)
			return nil
		}
		s.metrics.txsSent.Inc()
	}
	return nil
}

// confirm settles sent claims from their receipts.
func (s *Sender) confirm(ctx context.Context) error {
	claims, err := s.store.InFlight(ctx)
	if err != nil {
		return err
	}
	for _, c := range claims {
		receipt, err := s.rpc.TransactionReceipt(ctx, common.HexToHash(c.TxHash))
		if errors.Is(err, ethereum.NotFound) {
			if time.Since(c.SentAt.Time) > s.cfg.ConfirmTimeout && time.Since(s.resyncedAt) > s.cfg.ConfirmTimeout {
				s.needResync = true
			}
			continue
		}
		if err != nil {
			s.metrics.rpcErrors.Inc()
			return err
		}
		if err := s.settle(ctx, c, receipt); err != nil {
			return err
		}
	}
	return nil
}

// settle finishes a claim from its receipt. On Giga a failed receipt leaves
// its nonce unspent, so every transfer signed after it needs a resync.
func (s *Sender) settle(ctx context.Context, c *store.Claim, receipt *types.Receipt) error {
	if receipt.Status == types.ReceiptStatusSuccessful {
		s.metrics.claimTime.Observe(time.Since(c.CreatedAt).Seconds())
		return s.finish(ctx, c, store.StatusConfirmed, "")
	}
	s.needResync = true
	return s.finish(ctx, c, store.StatusFailed, "transfer rejected by the chain")
}

// resync settles the sent claims below the committed nonce and rebroadcasts
// the rest at the nonces they were signed at. A nonce no sent claim holds,
// left by a claim whose transfer was rejected, is filled with a zero-value
// transfer to the faucet so the transfers after it can land.
func (s *Sender) resync(ctx context.Context) error {
	committed, err := s.rpc.NonceAt(ctx, s.from, nil)
	if err != nil {
		return err
	}
	claims, err := s.store.InFlight(ctx)
	if err != nil {
		return err
	}
	next, stuck := committed, false
	for _, c := range claims {
		nonce := uint64(c.Nonce.Int64) //nolint:gosec // nonces are stored from a uint64
		if nonce < committed {
			if err := s.reconcile(ctx, c); err != nil {
				return err
			}
			continue
		}
		for ; next < nonce; next++ {
			if err := s.fill(ctx, next); err != nil {
				return err
			}
		}
		var tx types.Transaction
		if err := tx.UnmarshalBinary(c.RawTx); err != nil {
			return err
		}
		if err := s.rpc.SendTransaction(ctx, &tx); err != nil && !alreadyHave(err) {
			s.metrics.sendErrors.Inc()
			stuck = true
			slog.Warn("rebroadcast refused", "claim", c.ID, "nonce", nonce, "err", err)
		}
		next = max(next, nonce+1)
	}
	s.nonce = next
	s.needResync = stuck
	s.resyncedAt = time.Now()
	s.metrics.resyncs.Inc()
	s.metrics.nextNonce.Set(float64(s.nonce))
	slog.Info("nonce resynced", "committed", committed, "next", next)
	return nil
}

// alreadyHave reports whether a broadcast error means the node already has the
// transaction or has mined it.
func alreadyHave(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already known") || strings.Contains(msg, "already imported") || strings.Contains(msg, "nonce too low")
}

// reconcile settles a sent claim whose nonce the chain has already spent.
func (s *Sender) reconcile(ctx context.Context, c *store.Claim) error {
	receipt, err := s.rpc.TransactionReceipt(ctx, common.HexToHash(c.TxHash))
	switch {
	case errors.Is(err, ethereum.NotFound):
		if time.Since(c.SentAt.Time) <= s.cfg.ConfirmTimeout {
			return nil
		}
		slog.Error("faucet nonce spent by a transaction it did not send", "nonce", c.Nonce.Int64, "claim", c.ID)
		return s.finish(ctx, c, store.StatusFailed, "nonce spent elsewhere")
	case err != nil:
		return err
	default:
		return s.settle(ctx, c, receipt)
	}
}

// fill spends nonce with a zero-value transfer to the faucet. Filling the same
// nonce again signs the identical transaction.
func (s *Sender) fill(ctx context.Context, nonce uint64) error {
	tx, err := types.SignNewTx(s.cfg.Key, s.signer, &types.DynamicFeeTx{
		ChainID:   s.cfg.ChainID,
		Nonce:     nonce,
		GasTipCap: s.cfg.GasTipCap,
		GasFeeCap: s.cfg.GasFeeCap,
		Gas:       transferGas,
		To:        &s.from,
		Value:     new(big.Int),
	})
	if err != nil {
		return err
	}
	slog.Warn("filling unspent nonce", "nonce", nonce)
	return s.rpc.SendTransaction(ctx, tx)
}

func (s *Sender) finish(ctx context.Context, c *store.Claim, status store.Status, msg string) error {
	s.metrics.claimsDone.WithLabelValues(string(status)).Inc()
	return s.store.Finish(ctx, c.ID, status, msg, time.Now())
}

// WatchBalance refreshes the faucet balance every interval until ctx ends.
func (s *Sender) WatchBalance(ctx context.Context, interval time.Duration) error {
	for {
		if balance, err := s.rpc.BalanceAt(ctx, s.from, nil); err != nil {
			s.metrics.rpcErrors.Inc()
			slog.Warn("balance check failed", "err", err)
		} else {
			s.mu.Lock()
			s.balance, s.balanceTime = balance, time.Now()
			s.mu.Unlock()
			f, _ := new(big.Float).SetInt(balance).Float64()
			s.metrics.balance.Set(f)
		}
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// timeoutRPC bounds every call, so one hung request cannot stall the sender.
type timeoutRPC struct {
	rpc     RPC
	timeout time.Duration
}

func (r timeoutRPC) NonceAt(ctx context.Context, account common.Address, block *big.Int) (uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.rpc.NonceAt(ctx, account, block)
}

func (r timeoutRPC) BalanceAt(ctx context.Context, account common.Address, block *big.Int) (*big.Int, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.rpc.BalanceAt(ctx, account, block)
}

func (r timeoutRPC) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.rpc.SendTransaction(ctx, tx)
}

func (r timeoutRPC) TransactionReceipt(ctx context.Context, hash common.Hash) (*types.Receipt, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return r.rpc.TransactionReceipt(ctx, hash)
}
