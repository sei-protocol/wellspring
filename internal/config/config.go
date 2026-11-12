// Package config reads wellspring's configuration from the environment.
package config

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

// MainnetChainID is pacific-1's EVM chain ID. wellspring is testnet-only and
// refuses to run against it.
const MainnetChainID = 1329

// Config is wellspring's runtime configuration.
type Config struct {
	ListenAddr  string
	MetricsAddr string
	RPCURL      string
	// ChainID is the only chain the faucet signs for; the RPC must serve it.
	ChainID    uint64
	DBPath     string
	PrivateKey *ecdsa.PrivateKey

	Amount *big.Int
	// MaxRecipientBalance refuses a claim to an address already holding at least this much.
	MaxRecipientBalance *big.Int
	// MinFaucetBalance is the balance below which the faucet reports not ready.
	MinFaucetBalance *big.Int
	AddressCooldown  time.Duration
	IPWindow         time.Duration
	IPLimit          int
	MaxQueued        int
	TrustProxyHops   int

	CaptchaProvider string // turnstile, hcaptcha or none
	CaptchaSiteKey  string
	CaptchaSecret   string

	BatchInterval  time.Duration
	BatchSize      int
	GasTipCap      *big.Int
	GasFeeCap      *big.Int
	ConfirmTimeout time.Duration
}

// Load reads Config through getenv, usually os.LookupEnv.
func Load(getenv func(string) (string, bool)) (*Config, error) {
	r := reader{getenv: getenv}
	c := &Config{
		ListenAddr:          r.str("LISTEN_ADDR", ":8080"),
		MetricsAddr:         r.str("METRICS_ADDR", ":9090"),
		RPCURL:              r.str("RPC_URL", ""),
		DBPath:              r.str("DB_PATH", "wellspring.db"),
		ChainID:             r.uint64("CHAIN_ID", 0),
		PrivateKey:          r.key(),
		Amount:              r.wei("AMOUNT_WEI", "1000000000000000000"),
		MaxRecipientBalance: r.wei("MAX_RECIPIENT_BALANCE_WEI", "10000000000000000000"),
		MinFaucetBalance:    r.wei("MIN_FAUCET_BALANCE_WEI", "1000000000000000000000"),
		AddressCooldown:     r.duration("ADDRESS_COOLDOWN", 24*time.Hour),
		IPWindow:            r.duration("IP_WINDOW", 24*time.Hour),
		IPLimit:             r.int("IP_LIMIT", 5),
		MaxQueued:           r.int("MAX_QUEUED", 1000),
		TrustProxyHops:      r.int("TRUST_PROXY_HOPS", 0),
		CaptchaProvider:     r.str("CAPTCHA_PROVIDER", "turnstile"),
		CaptchaSiteKey:      r.str("CAPTCHA_SITE_KEY", ""),
		CaptchaSecret:       r.str("CAPTCHA_SECRET", ""),
		BatchInterval:       r.duration("BATCH_INTERVAL", 500*time.Millisecond),
		BatchSize:           r.int("BATCH_SIZE", 100),
		GasTipCap:           r.wei("GAS_TIP_CAP_WEI", "1000000000"),
		GasFeeCap:           r.wei("GAS_FEE_CAP_WEI", "10000000000"),
		ConfirmTimeout:      r.duration("CONFIRM_TIMEOUT", 30*time.Second),
	}
	r.check(c.RPCURL != "", "RPC_URL is required")
	r.check(c.ChainID != 0, "CHAIN_ID is required")
	r.check(c.ChainID != MainnetChainID, "CHAIN_ID 1329 is Sei mainnet; wellspring is testnet-only")
	switch c.CaptchaProvider {
	case "turnstile", "hcaptcha":
		r.check(c.CaptchaSiteKey != "" && c.CaptchaSecret != "", "CAPTCHA_SITE_KEY and CAPTCHA_SECRET are required for "+c.CaptchaProvider)
	case "none":
	default:
		r.check(false, fmt.Sprintf("CAPTCHA_PROVIDER %q is not turnstile, hcaptcha or none", c.CaptchaProvider))
	}
	r.check(c.Amount.Sign() > 0, "AMOUNT_WEI must be positive")
	r.check(c.GasFeeCap.Cmp(c.GasTipCap) >= 0, "GAS_FEE_CAP_WEI must be at least GAS_TIP_CAP_WEI")
	r.check(c.IPLimit > 0 && c.MaxQueued > 0 && c.BatchSize > 0, "IP_LIMIT, MAX_QUEUED and BATCH_SIZE must be positive")
	r.check(c.TrustProxyHops >= 0, "TRUST_PROXY_HOPS must not be negative")
	r.check(c.AddressCooldown > 0 && c.IPWindow > 0 && c.BatchInterval > 0 && c.ConfirmTimeout > 0,
		"ADDRESS_COOLDOWN, IP_WINDOW, BATCH_INTERVAL and CONFIRM_TIMEOUT must be positive")
	return c, errors.Join(r.errs...)
}

type reader struct {
	getenv func(string) (string, bool)
	errs   []error
}

func (r *reader) check(ok bool, msg string) {
	if !ok {
		r.errs = append(r.errs, errors.New(msg))
	}
}

func (r *reader) str(name, def string) string {
	if v, ok := r.getenv(name); ok && v != "" {
		return v
	}
	return def
}

func (r *reader) int(name string, def int) int {
	v, err := strconv.Atoi(r.str(name, strconv.Itoa(def)))
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be an integer", name))
	}
	return v
}

func (r *reader) uint64(name string, def uint64) uint64 {
	v, err := strconv.ParseUint(r.str(name, strconv.FormatUint(def, 10)), 10, 64)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be an unsigned integer", name))
	}
	return v
}

func (r *reader) duration(name string, def time.Duration) time.Duration {
	v, err := time.ParseDuration(r.str(name, def.String()))
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s must be a duration", name))
	}
	return v
}

func (r *reader) wei(name, def string) *big.Int {
	v, ok := new(big.Int).SetString(r.str(name, def), 10)
	if !ok || v.Sign() < 0 || v.BitLen() > 256 {
		r.errs = append(r.errs, fmt.Errorf("%s must be a decimal wei amount below 2^256", name))
		return new(big.Int)
	}
	return v
}

// key reads the signer key from PRIVATE_KEY_FILE (a mounted secret) or
// PRIVATE_KEY, never both. Errors never include the value.
func (r *reader) key() *ecdsa.PrivateKey {
	hex, fromEnv := r.getenv("PRIVATE_KEY")
	path, fromFile := r.getenv("PRIVATE_KEY_FILE")
	fromEnv, fromFile = fromEnv && hex != "", fromFile && path != ""
	switch {
	case fromEnv && fromFile:
		r.check(false, "set only one of PRIVATE_KEY and PRIVATE_KEY_FILE")
		return nil
	case fromFile:
		b, err := os.ReadFile(path) //nolint:gosec // the operator sets the path
		if err != nil {
			r.check(false, "PRIVATE_KEY_FILE is not readable")
			return nil
		}
		hex = string(b)
	case !fromEnv:
		r.check(false, "PRIVATE_KEY or PRIVATE_KEY_FILE is required")
		return nil
	}
	k, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(hex), "0x"))
	if err != nil {
		r.check(false, "the private key must be a hex secp256k1 key")
		return nil
	}
	return k
}
