package sender

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sei-protocol/wellspring/internal/store"
)

var amount = big.NewInt(1e18)

// chain mimics Giga: it admits only the sender's exact next nonce, and a
// transfer to a rejected address lands with a failed receipt without spending
// its nonce, so every transfer queued behind it is dropped.
type chain struct {
	signer types.Signer

	mu       sync.Mutex // guards the fields below
	nonce    uint64
	pending  []*types.Transaction
	receipts map[common.Hash]*types.Receipt
	paid     map[common.Address]int
	refuse   int
	hang     bool
	rejected map[common.Address]bool
}

func newChain() *chain {
	return &chain{
		signer:   types.LatestSignerForChainID(big.NewInt(713715)),
		receipts: map[common.Hash]*types.Receipt{},
		paid:     map[common.Address]int{},
		rejected: map[common.Address]bool{},
	}
}

func (c *chain) NonceAt(ctx context.Context, _ common.Address, _ *big.Int) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hang {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return c.nonce, nil
}

func (c *chain) BalanceAt(context.Context, common.Address, *big.Int) (*big.Int, error) {
	return big.NewInt(1), nil
}

func (c *chain) SendTransaction(_ context.Context, tx *types.Transaction) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refuse > 0 {
		c.refuse--
		return errors.New("node unavailable")
	}
	for _, p := range c.pending {
		if p.Hash() == tx.Hash() {
			return errors.New("already known")
		}
	}
	if tx.Nonce() != c.nonce+uint64(len(c.pending)) {
		return errors.New("invalid nonce")
	}
	c.pending = append(c.pending, tx)
	return nil
}

func (c *chain) TransactionReceipt(_ context.Context, hash common.Hash) (*types.Receipt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.receipts[hash]; ok {
		return r, nil
	}
	return nil, ethereum.NotFound
}

func (c *chain) mine() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tx := range c.pending {
		if c.rejected[*tx.To()] {
			c.receipts[tx.Hash()] = &types.Receipt{Status: types.ReceiptStatusFailed}
			break
		}
		c.receipts[tx.Hash()] = &types.Receipt{Status: types.ReceiptStatusSuccessful}
		if tx.Value().Sign() > 0 {
			c.paid[*tx.To()]++
		}
		c.nonce++
	}
	c.pending = nil
}

func (c *chain) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending = nil
}

type harness struct {
	t     *testing.T
	chain *chain
	store *store.Store
	cfg   Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wellspring.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key, _ := crypto.GenerateKey()
	return &harness{t: t, chain: newChain(), store: st, cfg: Config{
		ChainID:        big.NewInt(713715),
		Key:            key,
		GasTipCap:      big.NewInt(1),
		GasFeeCap:      big.NewInt(2),
		BatchSize:      3,
		ConfirmTimeout: time.Hour,
	}}
}

func (h *harness) sender() *Sender {
	return New(h.cfg, h.chain, h.store, prometheus.NewRegistry())
}

func (h *harness) claim(n int) []common.Address {
	h.t.Helper()
	var addrs []common.Address
	for i := range n {
		addr := common.BigToAddress(big.NewInt(int64(i + 1)))
		if _, _, err := h.store.CreateClaim(context.Background(), addr, "ip", amount,
			store.Limits{AddressCooldown: time.Hour, IPWindow: time.Hour, IPLimit: n, MaxQueued: n}, time.Now()); err != nil {
			h.t.Fatal(err)
		}
		addrs = append(addrs, addr)
	}
	return addrs
}

// drain steps the sender and mines until no claim is queued or sent.
func (h *harness) drain(s *Sender) {
	h.t.Helper()
	ctx := context.Background()
	for range 50 {
		s.step(ctx)
		h.chain.mine()
		s.step(ctx)
		queued, _ := h.store.Queued(ctx, 1)
		inflight, _ := h.store.InFlight(ctx)
		if len(queued) == 0 && len(inflight) == 0 {
			return
		}
	}
	h.t.Fatal("claims did not drain")
}

// paidOnce checks every address in want was paid exactly once and no other was.
func (h *harness) paidOnce(want ...common.Address) {
	h.t.Helper()
	if len(h.chain.paid) != len(want) {
		h.t.Fatalf("paid %d addresses, want %d: %v", len(h.chain.paid), len(want), h.chain.paid)
	}
	for _, a := range want {
		if h.chain.paid[a] != 1 {
			h.t.Fatalf("%s paid %d times", a, h.chain.paid[a])
		}
	}
}

func TestSendsEveryClaimOnce(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(7)
	s := h.sender()
	h.drain(s)
	h.paidOnce(addrs...)
	if h.chain.nonce != 7 || s.nonce != 7 {
		t.Fatalf("chain nonce %d, sender nonce %d", h.chain.nonce, s.nonce)
	}
}

func TestRefusedBroadcastResyncs(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(7)
	h.chain.refuse = 2
	h.drain(h.sender())
	h.paidOnce(addrs...)
}

func TestFailedReceiptFillsItsNonce(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(7)
	h.chain.rejected[addrs[1]] = true
	h.drain(h.sender())
	h.paidOnce(append([]common.Address{addrs[0]}, addrs[2:]...)...)
	if h.chain.nonce != 7 {
		t.Fatalf("chain nonce %d, want 7 (6 transfers and a filler)", h.chain.nonce)
	}
}

func TestDroppedTransactionsResend(t *testing.T) {
	h := newHarness(t)
	h.cfg.ConfirmTimeout = 0
	addrs := h.claim(3)
	s := h.sender()
	s.step(context.Background())
	h.chain.drop()
	h.drain(s)
	h.paidOnce(addrs...)
}

func TestRestartReconcilesInFlight(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(5)
	h.sender().step(context.Background())
	h.chain.mine() // the first batch lands while the faucet is down
	h.drain(h.sender())
	h.paidOnce(addrs...)
}

func TestPendingTransactionsLandAfterRestart(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(5)
	h.sender().step(context.Background())
	s := h.sender()
	s.step(context.Background())
	h.chain.mine() // the first batch was still pending when the restarted faucet resynced
	h.drain(s)
	h.paidOnce(addrs...)
}

func TestHungRPCTimesOut(t *testing.T) {
	h := newHarness(t)
	h.chain.hang = true
	s := h.sender()
	s.rpc.timeout = time.Millisecond
	done := make(chan struct{})
	go func() {
		s.step(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("step blocked on a hung RPC")
	}
}

func TestRefusedRebroadcastKeepsResyncing(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(1)
	h.sender().step(context.Background())
	h.chain.drop()
	h.chain.refuse = 2
	s := h.sender()
	s.step(context.Background())
	if !s.needResync {
		t.Fatal("resync gave up on a refused rebroadcast")
	}
	h.drain(s)
	h.paidOnce(addrs...)
}
