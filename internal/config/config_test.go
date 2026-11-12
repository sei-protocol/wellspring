package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfave/cli/v3"
)

const testKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, args ...string) (*Config, error) {
	t.Helper()
	var cfg *Config
	var err error
	cmd := &cli.Command{Name: "wellspring", Flags: Flags(), Action: func(_ context.Context, cmd *cli.Command) error {
		cfg, err = FromCommand(cmd)
		return nil
	}}
	if runErr := cmd.Run(context.Background(), append([]string{"wellspring"}, args...)); runErr != nil {
		t.Fatal(runErr)
	}
	return cfg, err
}

func valid(t *testing.T, extra ...string) []string {
	t.Helper()
	return append([]string{
		"--rpc-url", "http://localhost:8545",
		"--chain-id", "713715",
		"--private-key-file", writeFile(t, "0x"+testKey+"\n"),
		"--captcha-provider", "none",
	}, extra...)
}

func TestDefaults(t *testing.T) {
	cfg, err := load(t, valid(t)...)
	if err != nil {
		t.Fatal(err)
	}
	k, _ := crypto.HexToECDSA(testKey)
	if crypto.PubkeyToAddress(cfg.PrivateKey.PublicKey) != crypto.PubkeyToAddress(k.PublicKey) {
		t.Fatal("wrong signer key")
	}
	if cfg.ChainID != 713715 || cfg.ListenAddr != ":8080" || cfg.LogLevel != slog.LevelInfo ||
		cfg.Amount.String() != "1000000000000000000" || cfg.AddressCooldown != 24*time.Hour ||
		cfg.IPLimit != 5 || cfg.GasFeeCap.String() != "10000000000" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestEnvironment(t *testing.T) {
	t.Setenv(EnvVar("rpc-url"), "http://rpc:8545")
	t.Setenv(EnvVar("chain-id"), "713715")
	t.Setenv(EnvVar("private-key-file"), writeFile(t, testKey))
	t.Setenv(EnvVar("captcha-provider"), "turnstile")
	t.Setenv(EnvVar("captcha-site-key"), "site")
	t.Setenv(EnvVar("captcha-secret-file"), writeFile(t, " captcha-secret\n"))
	t.Setenv(EnvVar("ip-window"), "1h")
	t.Setenv(EnvVar("log-level"), "debug")
	cfg, err := load(t, "--ip-limit", "9")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RPCURL != "http://rpc:8545" || cfg.CaptchaSecret != "captcha-secret" || cfg.IPWindow != time.Hour ||
		cfg.IPLimit != 9 || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("environment not applied: %+v", cfg)
	}
	if EnvVar("max-recipient-balance-wei") != "WELLSPRING_MAX_RECIPIENT_BALANCE_WEI" {
		t.Fatal(EnvVar("max-recipient-balance-wei"))
	}
}

func TestInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"mainnet", []string{"--chain-id", "1329"}, "testnet-only"},
		{"no rpc", []string{"--rpc-url", ""}, "--rpc-url is required"},
		{"no chain id", []string{"--chain-id", "0"}, "--chain-id is required"},
		{"no key", []string{"--private-key-file", ""}, "--private-key-file is required"},
		{"missing key file", []string{"--private-key-file", "/nonexistent"}, "--private-key-file is not readable"},
		{"bad key", []string{"--private-key-file", writeFile(t, "not-a-key")}, "hex secp256k1 key"},
		{"unknown captcha", []string{"--captcha-provider", "recaptcha"}, "is not turnstile"},
		{"captcha without secret", []string{"--captcha-provider", "hcaptcha", "--captcha-site-key", "s"}, "--captcha-secret-file is required"},
		{"captcha without site key", []string{"--captcha-provider", "turnstile", "--captcha-secret-file", writeFile(t, "x")}, "--captcha-site-key is required"},
		{"empty captcha secret", []string{"--captcha-provider", "turnstile", "--captcha-site-key", "s", "--captcha-secret-file", writeFile(t, " \n")}, "is empty"},
		{"zero amount", []string{"--amount-wei", "0"}, "--amount-wei must be positive"},
		{"zero max recipient balance", []string{"--max-recipient-balance-wei", "0"}, "--max-recipient-balance-wei must be positive"},
		{"oversized amount", []string{"--amount-wei", "1" + strings.Repeat("0", 78)}, "decimal uint256"},
		{"negative amount", []string{"--amount-wei", "-1"}, "decimal uint256"},
		{"fee below tip", []string{"--gas-fee-cap-wei", "1", "--gas-tip-cap-wei", "2"}, "at least --gas-tip-cap-wei"},
		{"zero limit", []string{"--ip-limit", "0"}, "must be positive"},
		{"negative hops", []string{"--trust-proxy-hops", "-1"}, "must not be negative"},
		{"zero duration", []string{"--confirm-timeout", "0s"}, "--confirm-timeout must be positive"},
		{"bad log level", []string{"--log-level", "loud"}, "--log-level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, valid(t, tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestErrorsHideSecrets(t *testing.T) {
	_, err := load(t, valid(t, "--private-key-file", writeFile(t, testKey+"zz"), "--chain-id", "1329")...)
	if err == nil || strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "testnet-only") {
		t.Fatalf("err %v", err)
	}
}
