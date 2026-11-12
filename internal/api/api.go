// Package api serves the faucet's HTTP API and web page.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/sei-protocol/wellspring/internal/config"
	"github.com/sei-protocol/wellspring/internal/store"
)

// BalanceMaxAge is how old the faucet's last balance reading may be before
// the faucet reports not ready.
const BalanceMaxAge = time.Minute

const csp = "default-src 'self'; " +
	"script-src 'self' https://challenges.cloudflare.com https://js.hcaptcha.com https://*.hcaptcha.com; " +
	"frame-src https://challenges.cloudflare.com https://*.hcaptcha.com; " +
	"style-src 'self' 'unsafe-inline' https://*.hcaptcha.com; " +
	"connect-src 'self' https://*.hcaptcha.com; frame-ancestors 'none'"

//go:embed web
var web embed.FS

// Balances reads account balances.
type Balances interface {
	BalanceAt(ctx context.Context, account common.Address, block *big.Int) (*big.Int, error)
}

// Faucet reports the faucet's own state.
type Faucet interface {
	Address() common.Address
	Balance() (*big.Int, time.Time)
}

// Server is the faucet HTTP API.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	balances Balances
	faucet   Faucet
	captcha  Verifier
	requests *prometheus.CounterVec
}

// New returns a Server registering its metrics on reg.
func New(cfg *config.Config, st *store.Store, balances Balances, faucet Faucet, captcha Verifier, reg prometheus.Registerer) *Server {
	return &Server{
		cfg: cfg, store: st, balances: balances, faucet: faucet, captcha: captcha,
		requests: promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
			Name: "wellspring_claim_requests_total", Help: "Claim requests by outcome.",
		}, []string{"outcome"}),
	}
}

// Handler returns the server's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static := http.FileServerFS(web)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/web/"
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/web/app.js"
		static.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("POST /api/claims", s.createClaim)
	mux.HandleFunc("GET /api/claims/{id}", s.getClaim)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", s.ready)
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	balance, _ := s.faucet.Balance()
	writeJSON(w, http.StatusOK, map[string]any{
		"chainId":                s.cfg.ChainID,
		"chainName":              s.cfg.ChainName,
		"publicRpcUrl":           s.cfg.PublicRPCURL,
		"faucetAddress":          s.faucet.Address().Hex(),
		"amountWei":              s.cfg.Amount.String(),
		"balanceWei":             bigString(balance),
		"addressCooldownSeconds": int(s.cfg.AddressCooldown.Seconds()),
		"captchaProvider":        s.cfg.CaptchaProvider,
		"captchaSiteKey":         s.cfg.CaptchaSiteKey,
	})
}

type claimRequest struct {
	Address      string `json:"address"`
	CaptchaToken string `json:"captchaToken"`
}

func (s *Server) createClaim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		s.refuse(w, http.StatusBadRequest, "bad_request", "invalid JSON body", 0)
		return
	}
	if !strings.HasPrefix(req.Address, "0x") || !common.IsHexAddress(req.Address) {
		s.refuse(w, http.StatusBadRequest, "bad_address", "address must be a 0x-prefixed EVM address", 0)
		return
	}
	to := common.HexToAddress(req.Address)
	if to == (common.Address{}) || to == s.faucet.Address() {
		s.refuse(w, http.StatusBadRequest, "bad_address", "address cannot receive from the faucet", 0)
		return
	}
	ip := clientIP(r, s.cfg.TrustProxyHops)
	remoteIP := ""
	if ip != nil {
		remoteIP = ip.String()
	}
	if err := s.captcha.Verify(r.Context(), req.CaptchaToken, remoteIP); err != nil {
		if !errors.Is(err, errCaptcha) {
			slog.Warn("captcha verification unavailable", "err", err)
		}
		s.refuse(w, http.StatusForbidden, "captcha", "captcha verification failed", 0)
		return
	}
	held, err := s.balances.BalanceAt(r.Context(), to, nil)
	if err != nil {
		s.refuse(w, http.StatusServiceUnavailable, "rpc", "chain unavailable, try again shortly", time.Minute)
		return
	}
	if held.Cmp(s.cfg.MaxRecipientBalance) >= 0 {
		s.refuse(w, http.StatusConflict, "funded", "address already holds enough to test with", 0)
		return
	}
	claim, retryAfter, err := s.store.CreateClaim(r.Context(), to, rateKey(ip), s.cfg.Amount, store.Limits{
		AddressCooldown: s.cfg.AddressCooldown,
		IPWindow:        s.cfg.IPWindow,
		IPLimit:         s.cfg.IPLimit,
		MaxQueued:       s.cfg.MaxQueued,
	}, time.Now())
	switch {
	case errors.Is(err, store.ErrAddressCooldown), errors.Is(err, store.ErrIPLimit):
		s.refuse(w, http.StatusTooManyRequests, "rate_limited", err.Error(), retryAfter)
	case errors.Is(err, store.ErrQueueFull):
		s.refuse(w, http.StatusServiceUnavailable, "busy", err.Error(), retryAfter)
	case err != nil:
		slog.Error("create claim failed", "err", err)
		s.refuse(w, http.StatusInternalServerError, "internal", "internal error", 0)
	default:
		s.requests.WithLabelValues("accepted").Inc()
		writeJSON(w, http.StatusAccepted, claimView(claim))
	}
}

func (s *Server) getClaim(w http.ResponseWriter, r *http.Request) {
	claim, err := s.store.Get(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		slog.Error("get claim failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	case claim == nil:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
	default:
		writeJSON(w, http.StatusOK, claimView(claim))
	}
}

func (s *Server) ready(w http.ResponseWriter, _ *http.Request) {
	balance, at := s.faucet.Balance()
	switch {
	case balance == nil || time.Since(at) > BalanceMaxAge:
		http.Error(w, "chain unreachable", http.StatusServiceUnavailable)
	case balance.Cmp(s.cfg.MinFaucetBalance) < 0:
		http.Error(w, "faucet balance low", http.StatusServiceUnavailable)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func (s *Server) refuse(w http.ResponseWriter, status int, code, msg string, retryAfter time.Duration) {
	s.requests.WithLabelValues(code).Inc()
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
	}
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

func claimView(c *store.Claim) map[string]any {
	return map[string]any{
		"id":        c.ID,
		"address":   c.Address.Hex(),
		"amountWei": c.Amount.String(),
		"status":    c.Status,
		"txHash":    c.TxHash,
		"error":     c.Error,
		"createdAt": c.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// clientIP returns the address the outermost of hops trusted proxies saw the
// request come from, or the peer address when there are no trusted proxies.
func clientIP(r *http.Request, hops int) net.IP {
	if hops > 0 {
		var forwarded []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			for part := range strings.SplitSeq(h, ",") {
				forwarded = append(forwarded, strings.TrimSpace(part))
			}
		}
		if i := len(forwarded) - hops; i >= 0 {
			if ip := net.ParseIP(forwarded[i]); ip != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

// rateKey is the per-IP limit key: the IPv4 address, or the IPv6 /64, since a
// single IPv6 client usually controls a whole /64.
func rateKey(ip net.IP) string {
	if ip == nil {
		return "unknown"
	}
	if ip4 := ip.To4(); ip4 != nil {
		return ip4.String()
	}
	return (&net.IPNet{IP: ip.Mask(net.CIDRMask(64, 128)), Mask: net.CIDRMask(64, 128)}).String()
}

func bigString(v *big.Int) string {
	if v == nil {
		return ""
	}
	return v.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
