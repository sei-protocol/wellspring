package sender

import (
	"context"
	"database/sql"
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
	dto "github.com/prometheus/client_model/go"

	"github.com/sei-protocol/wellspring/internal/store"
)

var amount = big.NewInt(1e18)

// chain mimics Giga: it admits only the sender's exact next nonce, and a
// transfer to a rejected address lands with a failed receipt without spending
// its nonce, so every transfer queued behind it is dropped. A transaction whose
// fee cap is below minFee is refused.
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
	minFee   *big.Int
}

func newChain() *chain {
	return &chain{
		signer:   types.LatestSignerForChainID(big.NewInt(1330)),
		receipts: map[common.Hash]*types.Receipt{},
		paid:     map[common.Address]int{},
		rejected: map[common.Address]bool{},
		minFee:   new(big.Int),
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
	if tx.GasFeeCap().Cmp(c.minFee) < 0 {
		return errors.New("max fee per gas less than block base fee")
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
	path  string
	cfg   Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wellspring.db")
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key, _ := crypto.GenerateKey()
	return &harness{t: t, chain: newChain(), store: st, path: path, cfg: Config{
		ChainID:        big.NewInt(1330),
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

func TestFillAlreadyPendingKeepsResyncing(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(1)
	h.chain.rejected[addrs[0]] = true
	s := h.sender()
	ctx := context.Background()
	s.step(ctx)
	h.chain.mine()
	for range 2 {
		if err := s.resync(ctx); err != nil {
			t.Fatal(err)
		}
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

// TestResignedClaimSettlesOnce restarts the faucet with a raised fee cap after
// the node refuses a claim's transaction, and checks the claim is re-signed at
// its nonce and paid once, whichever of its transactions lands.
func TestResignedClaimSettlesOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		refuse      func(h *harness, old common.Hash)
		afterResign func(h *harness, hashes []common.Hash)
	}{
		"fee cap below base fee": {refuse: func(h *harness, _ common.Hash) {
			h.chain.drop()
			h.chain.minFee = big.NewInt(5)
		}},
		"old transaction lands after the re-sign": {refuse: func(h *harness, _ common.Hash) {
			h.chain.refuse = 1
		}},
		"old transaction fails after the re-sign": {
			refuse: func(h *harness, _ common.Hash) {
				h.chain.drop()
				h.chain.refuse = 1
			},
			afterResign: func(h *harness, hashes []common.Hash) {
				h.chain.receipts[hashes[0]] = &types.Receipt{Status: types.ReceiptStatusFailed}
			},
		},
		"re-signed transaction fails before the old one lands": {
			refuse: func(h *harness, _ common.Hash) { h.chain.refuse = 1 },
			afterResign: func(h *harness, hashes []common.Hash) {
				h.chain.receipts[hashes[1]] = &types.Receipt{Status: types.ReceiptStatusFailed}
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			addrs := h.claim(1)
			h.sender().step(ctx)
			sent, err := h.store.InFlight(ctx)
			if err != nil || len(sent) != 1 {
				t.Fatalf("in flight: %v %v", sent, err)
			}
			tc.refuse(h, common.HexToHash(sent[0].TxHash))

			h.cfg.GasFeeCap = big.NewInt(5)
			s := h.sender()
			s.step(ctx)
			hashes, err := h.store.TxHashes(ctx, sent[0].ID)
			if err != nil || len(hashes) != 2 || hashes[0] != common.HexToHash(sent[0].TxHash) {
				t.Fatalf("hashes after re-sign: %v %v", hashes, err)
			}
			if resigned, err := h.store.Get(ctx, sent[0].ID); err != nil || resigned.Nonce != sent[0].Nonce || common.HexToHash(resigned.TxHash) != hashes[1] {
				t.Fatalf("re-signed claim: %+v %v", resigned, err)
			}
			if tc.afterResign != nil {
				tc.afterResign(h, hashes)
			}

			h.drain(s)
			h.paidOnce(addrs...)
			if c, err := h.store.Get(ctx, sent[0].ID); err != nil || c.Status != store.StatusConfirmed {
				t.Fatalf("claim: %+v %v", c, err)
			}
			if h.chain.nonce != 1 {
				t.Fatalf("chain nonce %d, want 1", h.chain.nonce)
			}
		})
	}
}

// TestSettlesClaimWithoutHashHistory covers a claim sent by a binary that
// predates claim_txs, after a rollback past the migration.
func TestSettlesClaimWithoutHashHistory(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(1)
	h.sender().step(context.Background())
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), `DELETE FROM claim_txs`); err != nil {
		t.Fatal(err)
	}
	h.drain(h.sender())
	h.paidOnce(addrs...)
}

func TestGaugesTrackUnsettledClaimsAndBalance(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, _, err := h.store.CreateClaim(ctx, common.BigToAddress(big.NewInt(1)), "ip", amount,
		store.Limits{AddressCooldown: time.Hour, IPWindow: time.Hour, IPLimit: 1, MaxQueued: 1}, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	s := New(h.cfg, h.chain, h.store, reg)
	s.step(ctx) // sent, not yet mined
	if age := gauge(t, reg, "wellspring_oldest_unsettled_claim_seconds"); age < time.Hour.Seconds() {
		t.Fatalf("oldest unsettled %vs while a claim is sent, want at least an hour", age)
	}
	h.drain(s)
	if age := gauge(t, reg, "wellspring_oldest_unsettled_claim_seconds"); age != 0 {
		t.Fatalf("oldest unsettled %vs with every claim settled", age)
	}
	if families := gather(t, reg); families["wellspring_balance_wei"] != nil {
		t.Fatal("balance exported before it was read")
	}
}

func TestGrantsShareTheNonceOrder(t *testing.T) {
	h := newHarness(t)
	addrs := h.claim(2)
	granted := common.HexToAddress("0x00000000000000000000000000000000000ca201")
	big5 := new(big.Int).Mul(amount, big.NewInt(5))
	if _, _, err := h.store.CreateGrant(context.Background(), &store.Grant{
		Caller: "ns/sa", ID: "g", Address: granted, Request: "amount:5", Amount: big5, Reason: "r",
	}, new(big.Int).Mul(amount, big.NewInt(100)), time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	late := common.BigToAddress(big.NewInt(9))
	if _, _, err := h.store.CreateClaim(context.Background(), late, "ip2", amount,
		store.Limits{AddressCooldown: time.Hour, IPWindow: time.Hour, IPLimit: 1, MaxQueued: 10}, time.Now()); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	s := New(h.cfg, h.chain, h.store, reg)
	h.drain(s)
	h.paidOnce(addrs[0], addrs[1], granted, late)
	if h.chain.nonce != 4 {
		t.Fatalf("chain at nonce %d, want 4", h.chain.nonce)
	}
	g, _ := h.store.GetGrant(context.Background(), "ns/sa", "g")
	if g.Claim.Status != store.StatusConfirmed || g.Claim.Amount.Cmp(big5) != 0 {
		t.Fatalf("grant claim %+v", g.Claim)
	}
	if v := gather(t, reg)["wellspring_granted_wei_total"].GetMetric()[0].GetCounter().GetValue(); v != 5e18 {
		t.Fatalf("granted %v wei", v)
	}
	for _, m := range gather(t, reg)["wellspring_claims_finished_total"].GetMetric() {
		l := map[string]string{}
		for _, p := range m.GetLabel() {
			l[p.GetName()] = p.GetValue()
		}
		want := map[string]float64{"confirmed/claim": 3, "confirmed/grant": 1}[l["status"]+"/"+l["kind"]]
		if m.GetCounter().GetValue() != want {
			t.Fatalf("%v finished %v times, want %v", l, m.GetCounter().GetValue(), want)
		}
	}
}

func gather(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*dto.MetricFamily{}
	for _, f := range families {
		byName[f.GetName()] = f
	}
	return byName
}

func gauge(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	f := gather(t, reg)[name]
	if f == nil {
		t.Fatalf("%s not exported", name)
	}
	return f.GetMetric()[0].GetGauge().GetValue()
}
