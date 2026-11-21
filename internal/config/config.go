// Package config defines wellspring's flags and validates them into a Config.
// Every flag can also be set from a WELLSPRING_* environment variable.
package config

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfave/cli/v3"
)

// MainnetChainID is pacific-1's EVM chain ID. wellspring is testnet-only and
// refuses to run against it.
const MainnetChainID = 1329

// Config is wellspring's runtime configuration.
type Config struct {
	ListenAddr  string
	MetricsAddr string
	LogLevel    slog.Level
	RPCURL      string
	// ChainID is the only chain the faucet signs for; the RPC must serve it.
	ChainID uint64
	// ChainName and PublicRPCURL are what the web page offers wallets.
	ChainName    string
	PublicRPCURL string
	DBPath       string
	PrivateKey   *ecdsa.PrivateKey

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

	// AdminAddr serves the grants API; empty turns it off.
	AdminAddr string
	// AdminAudience is the audience a caller's ServiceAccount token must carry.
	AdminAudience string
	// AdminAllow lists the namespace/name ServiceAccounts, or namespace/* for
	// every one in a namespace, that may make grants.
	AdminAllow []string
	// GrantMax is the most one grant sends.
	GrantMax *big.Int
	// KubeAPIURL, KubeTokenFile and KubeCAFile reach the Kubernetes API that
	// reviews callers' tokens.
	KubeAPIURL    string
	KubeTokenFile string
	KubeCAFile    string

	BatchInterval  time.Duration
	BatchSize      int
	GasTipCap      *big.Int
	GasFeeCap      *big.Int
	ConfirmTimeout time.Duration
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// allowEntry matches a namespace/serviceaccount or namespace/* allowlist entry.
var allowEntry = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?/(\*|[a-z0-9]([-.a-z0-9]{0,251}[a-z0-9])?)$`)

// Flags returns wellspring's flags. Secrets are only read from files, so they
// stay out of the process environment and the pod spec.
func Flags() []cli.Flag {
	return []cli.Flag{
		str("listen-addr", ":8080", "HTTP listen address"),
		str("metrics-addr", ":9090", "Prometheus listen address"),
		str("log-level", "info", "debug, info, warn or error"),
		str("rpc-url", "", "EVM JSON-RPC endpoint; must serve --chain-id (required)"),
		&cli.Uint64Flag{Name: "chain-id", Usage: "EVM chain ID to sign for; 1329 (mainnet) is refused (required)", Sources: env("chain-id")},
		str("chain-name", "Sei testnet", "chain name shown on the page and offered to wallets"),
		str("public-rpc-url", "", "EVM RPC the page offers wallets; the add-network button is hidden when empty"),
		str("db-path", "wellspring.db", "SQLite database path"),
		file("private-key-file", "file holding the hex secp256k1 signer key (required)"),
		str("amount-wei", "1000000000000000000", "wei sent per claim"),
		str("max-recipient-balance-wei", "10000000000000000000", "refuse addresses already holding this much"),
		str("min-faucet-balance-wei", "1000000000000000000000", "below this balance the faucet reports not ready"),
		dur("address-cooldown", 24*time.Hour, "one claim per address per cooldown"),
		dur("ip-window", 24*time.Hour, "window for --ip-limit"),
		num("ip-limit", 5, "claims per client IP per --ip-window"),
		num("max-queued", 1000, "refuse new claims once this many wait"),
		num("trust-proxy-hops", 0, "reverse proxies in front that append to X-Forwarded-For"),
		str("captcha-provider", "turnstile", "turnstile, hcaptcha, or none for local use"),
		str("captcha-site-key", "", "public captcha site key"),
		file("captcha-secret-file", "file holding the captcha secret"),
		str("admin-addr", "", "grants API listen address; off when empty"),
		str("admin-audience", "wellspring", "audience a grant caller's ServiceAccount token must carry"),
		str("admin-allow", "", "comma-separated namespace/serviceaccount (or namespace/*) callers allowed to make grants"),
		str("grant-max-wei", "100000000000000000000000", "most wei one grant sends"),
		str("kube-api-url", "https://kubernetes.default.svc", "Kubernetes API that reviews grant callers' tokens"),
		str("kube-token-file", serviceAccountDir+"/token", "token wellspring authenticates to the Kubernetes API with"),
		str("kube-ca-file", serviceAccountDir+"/ca.crt", "CA bundle of the Kubernetes API"),
		dur("batch-interval", 500*time.Millisecond, "how often queued claims are sent"),
		num("batch-size", 100, "most claims sent per interval"),
		str("gas-tip-cap-wei", "1000000000", "EIP-1559 tip cap"),
		str("gas-fee-cap-wei", "10000000000", "EIP-1559 fee cap"),
		dur("confirm-timeout", 30*time.Second, "how long to wait for a receipt before resyncing the nonce"),
	}
}

// EnvVar returns the environment variable that sets flag name.
func EnvVar(name string) string {
	return "WELLSPRING_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func env(name string) cli.ValueSourceChain { return cli.EnvVars(EnvVar(name)) }

func str(name, value, usage string) cli.Flag {
	return &cli.StringFlag{Name: name, Value: value, Usage: usage, Sources: env(name)}
}

func file(name, usage string) cli.Flag {
	return &cli.StringFlag{Name: name, Usage: usage, Sources: env(name), TakesFile: true}
}

func dur(name string, value time.Duration, usage string) cli.Flag {
	return &cli.DurationFlag{Name: name, Value: value, Usage: usage, Sources: env(name)}
}

func num(name string, value int, usage string) cli.Flag {
	return &cli.IntFlag{Name: name, Value: value, Usage: usage, Sources: env(name)}
}

// FromCommand validates cmd's flags into a Config, reporting every problem at once.
func FromCommand(cmd *cli.Command) (*Config, error) {
	r := reader{cmd: cmd}
	c := &Config{
		ListenAddr:          cmd.String("listen-addr"),
		MetricsAddr:         cmd.String("metrics-addr"),
		LogLevel:            r.level("log-level"),
		RPCURL:              cmd.String("rpc-url"),
		ChainID:             cmd.Uint64("chain-id"),
		ChainName:           cmd.String("chain-name"),
		PublicRPCURL:        cmd.String("public-rpc-url"),
		DBPath:              cmd.String("db-path"),
		PrivateKey:          r.key(),
		Amount:              r.wei("amount-wei"),
		MaxRecipientBalance: r.wei("max-recipient-balance-wei"),
		MinFaucetBalance:    r.wei("min-faucet-balance-wei"),
		AddressCooldown:     r.positive("address-cooldown"),
		IPWindow:            r.positive("ip-window"),
		IPLimit:             cmd.Int("ip-limit"),
		MaxQueued:           cmd.Int("max-queued"),
		TrustProxyHops:      cmd.Int("trust-proxy-hops"),
		CaptchaProvider:     cmd.String("captcha-provider"),
		CaptchaSiteKey:      cmd.String("captcha-site-key"),
		AdminAddr:           cmd.String("admin-addr"),
		AdminAudience:       cmd.String("admin-audience"),
		GrantMax:            r.wei("grant-max-wei"),
		KubeAPIURL:          cmd.String("kube-api-url"),
		KubeTokenFile:       cmd.String("kube-token-file"),
		KubeCAFile:          cmd.String("kube-ca-file"),
		BatchInterval:       r.positive("batch-interval"),
		BatchSize:           cmd.Int("batch-size"),
		GasTipCap:           r.wei("gas-tip-cap-wei"),
		GasFeeCap:           r.wei("gas-fee-cap-wei"),
		ConfirmTimeout:      r.positive("confirm-timeout"),
	}
	r.check(c.RPCURL != "", "--rpc-url is required")
	r.check(c.ChainID != 0, "--chain-id is required")
	r.check(c.ChainID != MainnetChainID, "--chain-id 1329 is Sei mainnet; wellspring is testnet-only")
	switch c.CaptchaProvider {
	case "turnstile", "hcaptcha":
		c.CaptchaSecret = r.secret("captcha-secret-file")
		r.check(c.CaptchaSiteKey != "", "--captcha-site-key is required for "+c.CaptchaProvider)
	case "none":
	default:
		r.check(false, fmt.Sprintf("--captcha-provider %q is not turnstile, hcaptcha or none", c.CaptchaProvider))
	}
	r.check(c.Amount == nil || c.Amount.Sign() > 0, "--amount-wei must be positive")
	r.check(c.MaxRecipientBalance == nil || c.MaxRecipientBalance.Sign() > 0, "--max-recipient-balance-wei must be positive")
	r.check(c.GasFeeCap == nil || c.GasTipCap == nil || c.GasFeeCap.Cmp(c.GasTipCap) >= 0, "--gas-fee-cap-wei must be at least --gas-tip-cap-wei")
	r.check(c.IPLimit > 0 && c.MaxQueued > 0 && c.BatchSize > 0, "--ip-limit, --max-queued and --batch-size must be positive")
	r.check(c.TrustProxyHops >= 0, "--trust-proxy-hops must not be negative")
	if c.AdminAddr != "" {
		for _, e := range strings.Split(cmd.String("admin-allow"), ",") {
			if e = strings.TrimSpace(e); e != "" {
				r.check(allowEntry.MatchString(e), fmt.Sprintf("--admin-allow entry %q is not namespace/serviceaccount or namespace/*", e))
				c.AdminAllow = append(c.AdminAllow, e)
			}
		}
		r.check(len(c.AdminAllow) > 0, "--admin-allow is required with --admin-addr")
		r.check(c.AdminAudience != "", "--admin-audience is required with --admin-addr")
		r.check(c.KubeAPIURL != "", "--kube-api-url is required with --admin-addr")
		r.check(c.GrantMax == nil || c.GrantMax.Sign() > 0, "--grant-max-wei must be positive")
	}
	if len(r.errs) > 0 {
		return nil, errors.Join(r.errs...)
	}
	return c, nil
}

type reader struct {
	cmd  *cli.Command
	errs []error
}

func (r *reader) check(ok bool, msg string) {
	if !ok {
		r.errs = append(r.errs, errors.New(msg))
	}
}

func (r *reader) level(name string) slog.Level {
	var l slog.Level
	r.check(l.UnmarshalText([]byte(r.cmd.String(name))) == nil, "--"+name+" must be debug, info, warn or error")
	return l
}

func (r *reader) positive(name string) time.Duration {
	d := r.cmd.Duration(name)
	r.check(d > 0, "--"+name+" must be positive")
	return d
}

func (r *reader) wei(name string) *big.Int {
	v, ok := new(big.Int).SetString(r.cmd.String(name), 10)
	if !ok || v.Sign() < 0 || v.BitLen() > 256 {
		r.check(false, "--"+name+" must be a decimal uint256")
		return nil
	}
	return v
}

// secret reads the trimmed contents of the file flag name points at. Errors
// never include the contents.
func (r *reader) secret(name string) string {
	path := r.cmd.String(name)
	if path == "" {
		r.check(false, "--"+name+" is required")
		return ""
	}
	b, err := os.ReadFile(path) //nolint:gosec // the operator sets the path
	if err != nil {
		r.check(false, "--"+name+" is not readable")
		return ""
	}
	s := strings.TrimSpace(string(b))
	r.check(s != "", "--"+name+" is empty")
	return s
}

func (r *reader) key() *ecdsa.PrivateKey {
	hex := r.secret("private-key-file")
	if hex == "" {
		return nil
	}
	k, err := crypto.HexToECDSA(strings.TrimPrefix(hex, "0x"))
	if err != nil {
		r.check(false, "--private-key-file must hold a hex secp256k1 key")
		return nil
	}
	return k
}
