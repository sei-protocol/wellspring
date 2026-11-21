package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/ethereum/go-ethereum/common"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/sei-protocol/wellspring/internal/auth"
	"github.com/sei-protocol/wellspring/internal/store"
)

// Authenticator returns the namespace/name of the ServiceAccount a bearer
// token belongs to, or auth.ErrUnauthenticated or auth.ErrForbidden.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (string, error)
}

var grantID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

const maxReason = 256

type grantRequest struct {
	ID         string `json:"id"`
	Address    string `json:"address"`
	AmountWei  string `json:"amountWei"`
	TopUpToWei string `json:"topUpToWei"`
	Reason     string `json:"reason"`
}

// Admin serves the grants API: authenticated transfers of any amount up to the
// per-grant maximum, without the captcha, cooldowns or recipient balance cap of
// public claims. Grants go through the same store and sender as claims.
type Admin struct {
	*Server
	auth   Authenticator
	grants *prometheus.CounterVec
}

// NewAdmin returns the grants API of s.
func NewAdmin(s *Server, a Authenticator, reg prometheus.Registerer) *Admin {
	grants := promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "wellspring_grants_total", Help: "Grant requests by outcome.",
	}, []string{"outcome"})
	// From zero, so increase() sees the first of each.
	for _, outcome := range []string{"created", "auth_unavailable"} {
		grants.WithLabelValues(outcome)
	}
	return &Admin{Server: s, auth: a, grants: grants}
}

// Handler returns the admin HTTP handler.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/grants", a.authenticated(a.createGrant))
	mux.HandleFunc("GET /admin/grants/{id}", a.authenticated(a.getGrant))
	return securityHeaders(mux)
}

func (a *Admin) authenticated(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			a.refuseGrant(w, http.StatusUnauthorized, "unauthenticated", "a bearer ServiceAccount token is required")
			return
		}
		caller, err := a.auth.Authenticate(r.Context(), strings.TrimSpace(token))
		switch {
		case errors.Is(err, auth.ErrUnauthenticated):
			a.refuseGrant(w, http.StatusUnauthorized, "unauthenticated", err.Error())
		case errors.Is(err, auth.ErrForbidden):
			a.refuseGrant(w, http.StatusForbidden, "forbidden", err.Error())
		case err != nil:
			slog.Error("token review failed", "err", err)
			a.refuseGrant(w, http.StatusServiceUnavailable, "auth_unavailable", "cannot review the token, try again shortly")
		default:
			next(w, r, caller)
		}
	}
}

func (a *Admin) createGrant(w http.ResponseWriter, r *http.Request, caller string) {
	var req grantRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		a.refuseGrant(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !grantID.MatchString(req.ID) {
		a.refuseGrant(w, http.StatusBadRequest, "bad_request", "id must be 1-128 letters, digits, '.', '_', ':' or '-'")
		return
	}
	if req.Reason == "" || len(req.Reason) > maxReason || strings.IndexFunc(req.Reason, unicode.IsControl) >= 0 {
		a.refuseGrant(w, http.StatusBadRequest, "bad_request", "reason must be 1-256 bytes without control characters")
		return
	}
	if !strings.HasPrefix(req.Address, "0x") || !common.IsHexAddress(req.Address) {
		a.refuseGrant(w, http.StatusBadRequest, "bad_address", "address must be a 0x-prefixed EVM address")
		return
	}
	to := common.HexToAddress(req.Address)
	if to == (common.Address{}) || to == a.faucet.Address() {
		a.refuseGrant(w, http.StatusBadRequest, "bad_address", "address cannot receive from the faucet")
		return
	}
	var kind string
	var wei *big.Int
	switch {
	case req.AmountWei != "" && req.TopUpToWei == "":
		kind, wei = "amount:", parseWei(req.AmountWei)
	case req.TopUpToWei != "" && req.AmountWei == "":
		kind, wei = "top-up-to:", parseWei(req.TopUpToWei)
	}
	if wei == nil {
		a.refuseGrant(w, http.StatusBadRequest, "bad_amount", "set exactly one of amountWei and topUpToWei, as a positive decimal uint256")
		return
	}
	request := kind + wei.String()
	g := &store.Grant{Caller: caller, ID: req.ID, Address: to, Request: request, Reason: req.Reason, TopUp: req.TopUpToWei != ""}

	existing, err := a.store.GetGrant(r.Context(), caller, req.ID)
	if err != nil {
		slog.Error("read grant failed", "err", err)
		a.refuseGrant(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if existing != nil {
		a.answerExisting(w, g, existing)
		return
	}

	g.Amount = wei
	if req.TopUpToWei != "" {
		bctx, cancel := context.WithTimeout(r.Context(), rpcTimeout)
		held, err := a.balances.BalanceAt(bctx, to, nil)
		cancel()
		if err != nil {
			a.refuseGrant(w, http.StatusServiceUnavailable, "rpc", "chain unavailable, try again shortly")
			return
		}
		g.Amount = new(big.Int).Sub(wei, held)
		if g.Amount.Sign() < 0 {
			g.Amount.SetInt64(0)
		}
	}
	if g.Amount.Cmp(a.cfg.GrantMax) > 0 {
		a.refuseGrant(w, http.StatusBadRequest, "too_large", "grant exceeds the per-grant maximum of "+a.cfg.GrantMax.String()+" wei")
		return
	}
	balance, at := a.faucet.Balance()
	if balance == nil || time.Since(at) > BalanceMaxAge {
		a.refuseGrant(w, http.StatusServiceUnavailable, "rpc", "faucet balance unknown, try again shortly")
		return
	}
	available := new(big.Int).Sub(balance, a.cfg.MinFaucetBalance)

	created, isNew, err := a.store.CreateGrant(r.Context(), g, available, time.Now())
	switch {
	case errors.Is(err, store.ErrGrantNoFunds):
		a.refuseGrant(w, http.StatusServiceUnavailable, "no_funds", err.Error())
	case errors.Is(err, store.ErrGrantConflict):
		a.refuseGrant(w, http.StatusConflict, "conflict", err.Error())
	case err != nil:
		slog.Error("create grant failed", "err", err)
		a.refuseGrant(w, http.StatusInternalServerError, "internal", "internal error")
	case !isNew:
		a.answerExisting(w, g, created)
	default:
		a.grants.WithLabelValues("created").Inc()
		slog.Info("grant created", "caller", caller, "id", created.ID, "address", to.Hex(),
			"amount_wei", created.Amount.String(), "request", request, "reason", created.Reason)
		status := http.StatusAccepted
		if created.Claim == nil {
			status = http.StatusOK
		}
		writeJSON(w, status, grantView(created))
	}
}

// answerExisting answers a retried request with the grant it already made, or
// refuses one that reuses the grant's ID for a different request.
func (a *Admin) answerExisting(w http.ResponseWriter, req, existing *store.Grant) {
	if existing.Address != req.Address || existing.Request != req.Request {
		a.refuseGrant(w, http.StatusConflict, "conflict", store.ErrGrantConflict.Error())
		return
	}
	a.grants.WithLabelValues("repeated").Inc()
	writeJSON(w, http.StatusOK, grantView(existing))
}

func (a *Admin) getGrant(w http.ResponseWriter, r *http.Request, caller string) {
	g, err := a.store.GetGrant(r.Context(), caller, r.PathValue("id"))
	switch {
	case err != nil:
		slog.Error("read grant failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal", "message": "internal error"})
	case g == nil:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found", "message": "no such grant"})
	default:
		writeJSON(w, http.StatusOK, grantView(g))
	}
}

func (a *Admin) refuseGrant(w http.ResponseWriter, status int, code, msg string) {
	a.grants.WithLabelValues(code).Inc()
	writeJSON(w, status, map[string]string{"error": code, "message": msg})
}

// grantView reports a grant. A grant with nothing to send is confirmed at once.
func grantView(g *store.Grant) map[string]any {
	v := map[string]any{
		"id":        g.ID,
		"caller":    g.Caller,
		"address":   g.Address.Hex(),
		"amountWei": g.Amount.String(),
		"reason":    g.Reason,
		"status":    store.StatusConfirmed,
		"createdAt": g.CreatedAt.UTC().Format(time.RFC3339),
	}
	if c := g.Claim; c != nil {
		v["status"], v["txHash"], v["error"] = c.Status, c.TxHash, c.Error
	}
	return v
}

func parseWei(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() <= 0 || v.BitLen() > 256 {
		return nil
	}
	return v
}
