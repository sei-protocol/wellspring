package store

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func grant(id string, to common.Address, amount *big.Int) *Grant {
	return &Grant{Caller: "harpoon/setup", ID: id, Address: to, Request: "amount:" + amount.String(), Amount: amount, Reason: "test"}
}

func TestGrantIsIdempotent(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	big100 := new(big.Int).Mul(one, big.NewInt(100))
	first, created, err := s.CreateGrant(ctx, grant("g1", alice, one), big100, t0)
	if err != nil || !created || first.Claim == nil || first.Claim.Kind != KindGrant {
		t.Fatalf("first grant: %+v created=%v err=%v", first, created, err)
	}
	again, created, err := s.CreateGrant(ctx, grant("g1", alice, one), big100, t0)
	if err != nil || created || again.Claim.ID != first.Claim.ID {
		t.Fatalf("retry made a new grant: %+v created=%v err=%v", again, created, err)
	}
	if _, _, err := s.CreateGrant(ctx, grant("g1", bob, one), big100, t0); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("ID reused for another address: %v", err)
	}
	if _, _, err := s.CreateGrant(ctx, grant("g1", alice, big100), big100, t0); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("ID reused for another amount: %v", err)
	}
	other := grant("g1", alice, one)
	other.Caller = "other/sa"
	if _, created, err := s.CreateGrant(ctx, other, big100, t0); err != nil || !created {
		t.Fatalf("another caller's grant IDs are its own: created=%v err=%v", created, err)
	}
	queued, _ := s.Queued(ctx, 10)
	if len(queued) != 2 {
		t.Fatalf("%d claims queued, want 2", len(queued))
	}
	got, err := s.GetGrant(ctx, "harpoon/setup", "g1")
	if err != nil || got.Claim.ID != first.Claim.ID || got.Reason != "test" {
		t.Fatalf("GetGrant: %+v %v", got, err)
	}
	if got, _ := s.GetGrant(ctx, "harpoon/setup", "missing"); got != nil {
		t.Fatal("found a grant never made")
	}
}

func TestGrantConcurrentRetriesPayOnce(t *testing.T) {
	s, _ := open(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for range 20 {
		wg.Go(func() {
			_, isNew, err := s.CreateGrant(context.Background(), grant("g", alice, one), new(big.Int).Mul(one, big.NewInt(100)), t0)
			if err != nil {
				t.Error(err)
			}
			if isNew {
				mu.Lock()
				created++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	queued, _ := s.Queued(context.Background(), 100)
	if created != 1 || len(queued) != 1 {
		t.Fatalf("created %d grants and queued %d claims, want 1", created, len(queued))
	}
}

func TestGrantCountsUnsettledClaims(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	claim(t, s, bob, "ip", limits, t0)
	three := new(big.Int).Mul(one, big.NewInt(3))
	if _, _, err := s.CreateGrant(ctx, grant("a", alice, one), three, t0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateGrant(ctx, grant("b", alice, new(big.Int).Add(one, big.NewInt(1))), three, t0); !errors.Is(err, ErrGrantNoFunds) {
		t.Fatalf("grant spending past the floor with unsettled claims: %v", err)
	}
	if _, _, err := s.CreateGrant(ctx, grant("b", alice, one), three, t0); err != nil {
		t.Fatalf("grant spending exactly to the floor: %v", err)
	}
	nothing := grant("c", alice, new(big.Int))
	if g, created, err := s.CreateGrant(ctx, nothing, new(big.Int), t0); err != nil || !created || g.Claim != nil {
		t.Fatalf("grant with nothing to send: %+v %v %v", g, created, err)
	}
}

func TestGrantsSkipPublicCooldown(t *testing.T) {
	s, _ := open(t)
	if _, _, err := s.CreateGrant(context.Background(), grant("g", alice, one), one, t0); err != nil {
		t.Fatal(err)
	}
	claim(t, s, alice, "ip", limits, t0)
}
