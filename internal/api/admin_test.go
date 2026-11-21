package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/sei-protocol/wellspring/internal/auth"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, token string) (string, error) {
	switch token {
	case "ns-a", "ns-b":
		return token + "/sa", nil
	case "forbidden":
		return "", auth.ErrForbidden
	}
	return "", auth.ErrUnauthenticated
}

func (h *harness) grant(token, body string) (*httptest.ResponseRecorder, map[string]any) {
	return h.adminDo(http.MethodPost, "/admin/grants", token, body)
}

func (h *harness) adminDo(method, path, token, body string) (*httptest.ResponseRecorder, map[string]any) {
	r := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.admin.ServeHTTP(w, r)
	var v map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	return w, v
}

func grantBody(id, to, field, wei string) string {
	return `{"id":"` + id + `","address":"` + to + `","` + field + `":"` + wei + `","reason":"load test"}`
}

func TestGrantLifecycle(t *testing.T) {
	h := newHarness(t)
	body := grantBody("run-1", addr(1), "amountWei", sei(400).String())
	w, v := h.grant("ns-a", body)
	if w.Code != http.StatusAccepted || v["status"] != "queued" || v["amountWei"] != sei(400).String() || v["caller"] != "ns-a/sa" {
		t.Fatalf("grant: %d %v", w.Code, v)
	}
	// Grants skip the captcha and the recipient balance cap.
	h.captcha.err = errCaptcha
	h.balances.held[common.HexToAddress(addr(1))] = sei(1000)
	w, again := h.grant("ns-a", body)
	if w.Code != http.StatusOK || again["createdAt"] != v["createdAt"] {
		t.Fatalf("retry: %d %v", w.Code, again)
	}
	if w, _ := h.grant("ns-a", grantBody("run-1", addr(1), "amountWei", sei(5).String())); w.Code != http.StatusConflict {
		t.Fatalf("ID reused for a different amount: %d", w.Code)
	}
	if w, v := h.adminDo(http.MethodGet, "/admin/grants/run-1", "ns-a", ""); w.Code != http.StatusOK || v["status"] != "queued" {
		t.Fatalf("get: %d %v", w.Code, v)
	}
	if w, _ := h.adminDo(http.MethodGet, "/admin/grants/run-1", "ns-b", ""); w.Code != http.StatusNotFound {
		t.Fatalf("another caller read the grant: %d", w.Code)
	}
	if w, _ := h.grant("ns-a", grantBody("run-2", addr(2), "amountWei", sei(400).String())); w.Code != http.StatusAccepted {
		t.Fatalf("second grant: %d", w.Code)
	}
	// 1000 SEI balance, 100 SEI floor and 800 SEI promised.
	if w, v := h.grant("ns-a", grantBody("run-3", addr(3), "amountWei", new(big.Int).Add(sei(100), big.NewInt(1)).String())); w.Code != http.StatusServiceUnavailable || v["error"] != "no_funds" {
		t.Fatalf("grant past the floor: %d %v", w.Code, v)
	}
}

func TestGrantTopUp(t *testing.T) {
	h := newHarness(t)
	to := common.HexToAddress(addr(1))
	h.balances.held[to] = sei(30)
	if w, v := h.grant("ns-a", grantBody("t", addr(1), "topUpToWei", sei(100).String())); w.Code != http.StatusAccepted || v["amountWei"] != sei(70).String() {
		t.Fatalf("top-up: %d %v", w.Code, v)
	}
	// The retry finds the grant even though the balance it was computed from moved.
	h.balances.held[to] = sei(100)
	if w, v := h.grant("ns-a", grantBody("t", addr(1), "topUpToWei", sei(100).String())); w.Code != http.StatusOK || v["amountWei"] != sei(70).String() {
		t.Fatalf("top-up retry: %d %v", w.Code, v)
	}
	if w, v := h.grant("ns-a", grantBody("t2", addr(1), "topUpToWei", sei(50).String())); w.Code != http.StatusServiceUnavailable || v["error"] != "busy" {
		t.Fatalf("top-up while a transfer to the address is queued: %d %v", w.Code, v)
	}
	h.balances.held[common.HexToAddress(addr(2))] = sei(100)
	if w, v := h.grant("ns-a", grantBody("t3", addr(2), "topUpToWei", sei(50).String())); w.Code != http.StatusOK || v["amountWei"] != "0" || v["status"] != "confirmed" {
		t.Fatalf("top-up of a funded address: %d %v", w.Code, v)
	}
}

func TestGrantRefusals(t *testing.T) {
	h := newHarness(t)
	for name, tc := range map[string]struct {
		token, body string
		code        int
	}{
		"no token":      {"", grantBody("a", addr(1), "amountWei", "1"), http.StatusUnauthorized},
		"bad token":     {"nope", grantBody("a", addr(1), "amountWei", "1"), http.StatusUnauthorized},
		"not allowed":   {"forbidden", grantBody("a", addr(1), "amountWei", "1"), http.StatusForbidden},
		"bad id":        {"ns-a", grantBody("a/b", addr(1), "amountWei", "1"), http.StatusBadRequest},
		"no reason":     {"ns-a", `{"id":"a","address":"` + addr(1) + `","amountWei":"1"}`, http.StatusBadRequest},
		"faucet":        {"ns-a", grantBody("a", faucetAddr.Hex(), "amountWei", "1"), http.StatusBadRequest},
		"zero address":  {"ns-a", grantBody("a", addr(0), "amountWei", "1"), http.StatusBadRequest},
		"zero amount":   {"ns-a", grantBody("a", addr(1), "amountWei", "0"), http.StatusBadRequest},
		"negative":      {"ns-a", grantBody("a", addr(1), "amountWei", "-1"), http.StatusBadRequest},
		"both amounts":  {"ns-a", `{"id":"a","address":"` + addr(1) + `","amountWei":"1","topUpToWei":"1","reason":"r"}`, http.StatusBadRequest},
		"over the max":  {"ns-a", grantBody("a", addr(1), "amountWei", new(big.Int).Add(sei(500), big.NewInt(1)).String()), http.StatusBadRequest},
		"over uint256":  {"ns-a", grantBody("a", addr(1), "amountWei", new(big.Int).Lsh(big.NewInt(1), 256).String()), http.StatusBadRequest},
		"control chars": {"ns-a", `{"id":"a","address":"` + addr(1) + `","amountWei":"1","reason":"a\nb"}`, http.StatusBadRequest},
	} {
		if w, _ := h.grant(tc.token, tc.body); w.Code != tc.code {
			t.Errorf("%s: got %d, want %d: %s", name, w.Code, tc.code, w.Body)
		}
	}
	h.balances.err = errors.New("rpc down")
	if w, v := h.grant("ns-a", grantBody("a", addr(1), "amountWei", "1")); w.Code != http.StatusServiceUnavailable || v["error"] != "rpc" {
		t.Fatalf("grant without the faucet balance: %d %v", w.Code, v)
	}
}
