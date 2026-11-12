package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
)

const testKey = "4c0883a69102937d6231471b5dbb6204fe5129617082792ae468d01a3f362318"

func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}

func valid() map[string]string {
	return map[string]string{
		"RPC_URL":          "http://localhost:8545",
		"CHAIN_ID":         "713715",
		"PRIVATE_KEY":      "0x" + testKey,
		"CAPTCHA_PROVIDER": "none",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(lookup(valid()))
	if err != nil {
		t.Fatal(err)
	}
	k, _ := crypto.HexToECDSA(testKey)
	want := crypto.PubkeyToAddress(k.PublicKey)
	if got := crypto.PubkeyToAddress(c.PrivateKey.PublicKey); got != want {
		t.Fatalf("key address %s, want %s", got, want)
	}
	if c.ChainID != 713715 || c.Amount.String() != "1000000000000000000" || c.AddressCooldown != 24*time.Hour || c.IPLimit != 5 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := valid()
	delete(env, "PRIVATE_KEY")
	env["PRIVATE_KEY_FILE"] = path
	if _, err := Load(lookup(env)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejects(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		env        map[string]string
	}{
		{"mainnet", "testnet-only", map[string]string{"CHAIN_ID": "1329"}},
		{"no rpc", "RPC_URL is required", map[string]string{"RPC_URL": ""}},
		{"no chain", "CHAIN_ID is required", map[string]string{"CHAIN_ID": ""}},
		{"bad chain", "CHAIN_ID must be", map[string]string{"CHAIN_ID": "-1"}},
		{"no key", "PRIVATE_KEY or PRIVATE_KEY_FILE is required", map[string]string{"PRIVATE_KEY": ""}},
		{"bad key", "hex secp256k1 key", map[string]string{"PRIVATE_KEY": "0xnotakey"}},
		{"two keys", "only one of", map[string]string{"PRIVATE_KEY_FILE": "/k"}},
		{"missing key file", "not readable", map[string]string{"PRIVATE_KEY": "", "PRIVATE_KEY_FILE": "/does/not/exist"}},
		{"captcha secret", "CAPTCHA_SECRET are required", map[string]string{"CAPTCHA_PROVIDER": "turnstile"}},
		{"captcha provider", "is not turnstile", map[string]string{"CAPTCHA_PROVIDER": "recaptcha"}},
		{"zero amount", "AMOUNT_WEI must be positive", map[string]string{"AMOUNT_WEI": "0"}},
		{"huge amount", "below 2^256", map[string]string{"AMOUNT_WEI": "1" + strings.Repeat("0", 78)}},
		{"fee caps", "at least GAS_TIP_CAP_WEI", map[string]string{"GAS_TIP_CAP_WEI": "2", "GAS_FEE_CAP_WEI": "1"}},
		{"ip limit", "must be positive", map[string]string{"IP_LIMIT": "0"}},
		{"proxy hops", "must not be negative", map[string]string{"TRUST_PROXY_HOPS": "-1"}},
		{"cooldown", "must be positive", map[string]string{"ADDRESS_COOLDOWN": "0s"}},
		{"bad duration", "IP_WINDOW must be a duration", map[string]string{"IP_WINDOW": "a day"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := valid()
			for k, v := range tc.env {
				env[k] = v
			}
			_, err := Load(lookup(env))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error containing %q", err, tc.want)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Fatal("error leaks the private key")
			}
		})
	}
}
