package api

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/sei-protocol/wellspring/internal/config"
	"github.com/sei-protocol/wellspring/internal/store"
)

var faucetAddr = common.HexToAddress("0xfa00000000000000000000000000000000000001")

type fakeFaucet struct {
	balance *big.Int
	at      time.Time
}

func (f *fakeFaucet) Address() common.Address        { return faucetAddr }
func (f *fakeFaucet) Balance() (*big.Int, time.Time) { return f.balance, f.at }

type fakeBalances struct {
	held map[common.Address]*big.Int
	err  error
	hang bool
}

func (b *fakeBalances) BalanceAt(ctx context.Context, a common.Address, _ *big.Int) (*big.Int, error) {
	if b.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if b.err != nil {
		return nil, b.err
	}
	if v, ok := b.held[a]; ok {
		return v, nil
	}
	return new(big.Int), nil
}

type fakeCaptcha struct{ err error }

func (c *fakeCaptcha) Verify(context.Context, string, string) error { return c.err }

type harness struct {
	srv      http.Handler
	faucet   *fakeFaucet
	balances *fakeBalances
	captcha  *fakeCaptcha
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "wellspring.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		ChainID: 713715, ChainName: "Giga testnet",
		Amount: sei(1), MaxRecipientBalance: sei(10), MinFaucetBalance: sei(100),
		AddressCooldown: 24 * time.Hour, IPWindow: 24 * time.Hour, IPLimit: 2, MaxQueued: 100,
		CaptchaProvider: "turnstile", CaptchaSiteKey: "site",
	}
	h := &harness{
		faucet:   &fakeFaucet{balance: sei(1000), at: time.Now()},
		balances: &fakeBalances{held: map[common.Address]*big.Int{}},
		captcha:  &fakeCaptcha{},
	}
	h.srv = New(cfg, st, h.balances, h.faucet, h.captcha, prometheus.NewRegistry()).Handler()
	return h
}

func (h *harness) do(method, path, body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.1:1234"
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.srv.ServeHTTP(w, r)
	return w
}

func sei(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

func claimBody(addr string) string { return `{"address":"` + addr + `","captchaToken":"t"}` }

func addr(i int64) string { return common.BigToAddress(big.NewInt(i)).Hex() }

func TestClaimLifecycle(t *testing.T) {
	h := newHarness(t)
	w := h.do("POST", "/api/claims", claimBody(addr(1)))
	if w.Code != http.StatusAccepted {
		t.Fatalf("claim: %d %s", w.Code, w.Body)
	}
	var claim map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}
	if claim["status"] != "queued" || claim["amountWei"] != "1000000000000000000" || claim["address"] != addr(1) {
		t.Fatalf("claim %v", claim)
	}
	if w := h.do("GET", "/api/claims/"+claim["id"].(string), ""); w.Code != http.StatusOK {
		t.Fatalf("get: %d", w.Code)
	}
	if w := h.do("GET", "/api/claims/missing", ""); w.Code != http.StatusNotFound {
		t.Fatalf("get missing: %d", w.Code)
	}
	w = h.do("POST", "/api/claims", claimBody(addr(1)))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("repeat claim: %d, Retry-After %q", w.Code, w.Header().Get("Retry-After"))
	}
}

func TestClaimRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*harness)
		body  string
		want  int
		code  string
	}{
		{"bad json", nil, `{`, http.StatusBadRequest, "bad_request"},
		{"no 0x", nil, claimBody(strings.TrimPrefix(addr(1), "0x")), http.StatusBadRequest, "bad_address"},
		{"short", nil, claimBody("0x1234"), http.StatusBadRequest, "bad_address"},
		{"zero", nil, claimBody(addr(0)), http.StatusBadRequest, "bad_address"},
		{"faucet", nil, claimBody(faucetAddr.Hex()), http.StatusBadRequest, "bad_address"},
		{"captcha", func(h *harness) { h.captcha.err = errCaptcha }, claimBody(addr(1)), http.StatusForbidden, "captcha"},
		{"captcha down", func(h *harness) { h.captcha.err = errors.New("timeout") }, claimBody(addr(1)), http.StatusForbidden, "captcha"},
		{"rpc down", func(h *harness) { h.balances.err = errors.New("down") }, claimBody(addr(1)), http.StatusServiceUnavailable, "rpc"},
		{"funded", func(h *harness) { h.balances.held[common.HexToAddress(addr(1))] = sei(10) }, claimBody(addr(1)), http.StatusConflict, "funded"},
		{"oversized", nil, claimBody(strings.Repeat("a", 5000)), http.StatusBadRequest, "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			w := h.do("POST", "/api/claims", tc.body)
			var body map[string]string
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != tc.want || body["error"] != tc.code {
				t.Fatalf("got %d %v, want %d %s", w.Code, body, tc.want, tc.code)
			}
		})
	}
}

func TestHungRPCTimesOut(t *testing.T) {
	defer func(d time.Duration) { rpcTimeout = d }(rpcTimeout)
	rpcTimeout = time.Millisecond
	h := newHarness(t)
	h.balances.hang = true
	if w := h.do("POST", "/api/claims", claimBody(addr(1))); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

func TestIPLimitCoversIPv6Prefix(t *testing.T) {
	h := newHarness(t)
	for i, ip := range []string{"[2001:db8::1]:1", "[2001:db8::2]:1", "[2001:db8::3]:1"} {
		r := httptest.NewRequestWithContext(context.Background(), "POST", "/api/claims", strings.NewReader(claimBody(addr(int64(i+1)))))
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		h.srv.ServeHTTP(w, r)
		if want := map[bool]int{true: http.StatusAccepted, false: http.StatusTooManyRequests}[i < 2]; w.Code != want {
			t.Fatalf("claim %d from %s: %d, want %d", i, ip, w.Code, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		hops int
		xff  []string
		want string
	}{
		{0, []string{"203.0.113.9"}, "192.0.2.1"},
		{1, []string{"198.51.100.1, 203.0.113.9"}, "203.0.113.9"},
		{2, []string{"198.51.100.1", "203.0.113.9"}, "198.51.100.1"},
		{3, []string{"203.0.113.9"}, "192.0.2.1"},
		{1, []string{"not-an-ip"}, "192.0.2.1"},
	} {
		r := httptest.NewRequestWithContext(context.Background(), "GET", "/", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(r, tc.hops).String(); got != tc.want {
			t.Errorf("hops %d, XFF %v: %s, want %s", tc.hops, tc.xff, got, tc.want)
		}
	}
}

func TestReady(t *testing.T) {
	h := newHarness(t)
	if w := h.do("GET", "/readyz", ""); w.Code != http.StatusOK {
		t.Fatalf("ready: %d", w.Code)
	}
	h.faucet.balance = big.NewInt(1)
	if w := h.do("GET", "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("low balance: %d", w.Code)
	}
	h.faucet.balance, h.faucet.at = sei(1000), time.Now().Add(-2*BalanceMaxAge)
	if w := h.do("GET", "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("stale balance: %d", w.Code)
	}
	if w := h.do("GET", "/healthz", ""); w.Code != http.StatusOK {
		t.Fatalf("healthz: %d", w.Code)
	}
}

func TestPage(t *testing.T) {
	h := newHarness(t)
	w := h.do("GET", "/", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `src="/app.js"`) ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("index: %d %v", w.Code, w.Header())
	}
	if w := h.do("GET", "/app.js", ""); w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Fatalf("app.js: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := h.do("GET", "/web/app.js", ""); w.Code != http.StatusNotFound {
		t.Fatalf("/web/app.js: %d", w.Code)
	}
	var info map[string]any
	_ = json.Unmarshal(h.do("GET", "/api/info", "").Body.Bytes(), &info)
	if info["chainName"] != "Giga testnet" || info["faucetAddress"] != faucetAddr.Hex() {
		t.Fatalf("info %v", info)
	}
}
