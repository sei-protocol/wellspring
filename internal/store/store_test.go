package store

import (
	"context"
	"errors"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var (
	t0     = time.UnixMilli(1_800_000_000_000)
	one    = big.NewInt(1e18)
	limits = Limits{AddressCooldown: 24 * time.Hour, IPWindow: 24 * time.Hour, IPLimit: 2, MaxQueued: 10}
	alice  = common.HexToAddress("0x00000000000000000000000000000000000a11ce")
	bob    = common.HexToAddress("0x0000000000000000000000000000000000000b0b")
	carol  = common.HexToAddress("0x00000000000000000000000000000000000ca201")
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wellspring?.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func claim(t *testing.T, s *Store, addr common.Address, ip string, l Limits, now time.Time) *Claim {
	t.Helper()
	c, _, err := s.CreateClaim(context.Background(), addr, ip, one, l, now)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCreateClaimLimits(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	claim(t, s, alice, "1.1.1.1", limits, t0)

	_, wait, err := s.CreateClaim(ctx, alice, "2.2.2.2", one, limits, t0.Add(time.Hour))
	if !errors.Is(err, ErrAddressCooldown) || wait != 23*time.Hour {
		t.Fatalf("repeat address: wait %v, err %v", wait, err)
	}
	claim(t, s, alice, "2.2.2.2", limits, t0.Add(24*time.Hour+time.Millisecond))

	claim(t, s, bob, "1.1.1.1", limits, t0.Add(2*time.Hour))
	_, wait, err = s.CreateClaim(ctx, carol, "1.1.1.1", one, limits, t0.Add(3*time.Hour))
	if !errors.Is(err, ErrIPLimit) || wait != 21*time.Hour {
		t.Fatalf("ip limit: wait %v, err %v", wait, err)
	}

	full := limits
	full.MaxQueued = 3
	if _, _, err := s.CreateClaim(ctx, carol, "3.3.3.3", one, full, t0.Add(3*time.Hour)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("queue full: %v", err)
	}
}

func TestFailedClaimsDoNotCount(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	c := claim(t, s, alice, "1.1.1.1", Limits{AddressCooldown: time.Hour, IPWindow: time.Hour, IPLimit: 1, MaxQueued: 1}, t0)
	if err := s.MarkSent(ctx, c.ID, 0, common.Hash{1}, []byte{1}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish(ctx, c.ID, StatusFailed, "reverted", t0); err != nil {
		t.Fatal(err)
	}
	claim(t, s, alice, "1.1.1.1", Limits{AddressCooldown: time.Hour, IPWindow: time.Hour, IPLimit: 1, MaxQueued: 1}, t0)
}

func TestLifecyclePersists(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	a := claim(t, s, alice, "1.1.1.1", limits, t0)
	b := claim(t, s, bob, "2.2.2.2", limits, t0)

	if q, err := s.Queued(ctx, 10); err != nil || len(q) != 2 || q[0].ID != min(a.ID, b.ID) {
		t.Fatalf("queued: %v %v", q, err)
	}
	hash := common.HexToHash("0xabc")
	if err := s.MarkSent(ctx, a.ID, 7, hash, []byte{0xde, 0xad}, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSent(ctx, a.ID, 8, hash, nil, t0); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("double MarkSent: %v", err)
	}
	if err := s.Finish(ctx, b.ID, StatusConfirmed, "", t0); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("Finish of a queued claim: %v", err)
	}
	if err := s.Finish(ctx, a.ID, StatusQueued, "", t0); !errors.Is(err, ErrWrongStatus) {
		t.Fatalf("Finish to a non-final status: %v", err)
	}
	if n, err := s.CountQueued(ctx); err != nil || n != 1 {
		t.Fatalf("count queued: %d %v", n, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	inflight, err := s.InFlight(ctx)
	if err != nil || len(inflight) != 1 {
		t.Fatalf("in flight after reopen: %v %v", inflight, err)
	}
	got := inflight[0]
	if got.Address != alice || got.Amount.Cmp(one) != 0 || got.Nonce.Int64 != 7 || got.TxHash != hash.Hex() || string(got.RawTx) != "\xde\xad" || !got.SentAt.Valid {
		t.Fatalf("in flight claim: %+v", got)
	}
	if err := s.Finish(ctx, a.ID, StatusConfirmed, "", t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Get(ctx, a.ID); err != nil || c.Status != StatusConfirmed || !c.DoneAt.Valid {
		t.Fatalf("confirmed: %+v %v", c, err)
	}
	if c, err := s.Get(ctx, "missing"); err != nil || c != nil {
		t.Fatalf("missing claim: %v %v", c, err)
	}
}

func TestConcurrentClaimsForOneAddress(t *testing.T) {
	s, _ := open(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for range 20 {
		wg.Go(func() {
			if _, _, err := s.CreateClaim(context.Background(), alice, "1.1.1.1", one, limits, t0); err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("%d claims created for one address, want 1", created)
	}
}
