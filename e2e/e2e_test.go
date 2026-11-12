//go:build e2e

// Package e2e runs the wellspring binary against a real EVM node. Set
// WELLSPRING_E2E_RPC_URL and WELLSPRING_E2E_KEY_FILE (a funded signer key)
// and run `make e2e`. Other WELLSPRING_* variables pass through to the binary.
package e2e

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	claims = 30
	amount = 1_000_000_000_000_000 // 0.001 SEI
)

func TestFaucet(t *testing.T) {
	ctx := t.Context()
	rpcURL, keyFile := os.Getenv("WELLSPRING_E2E_RPC_URL"), os.Getenv("WELLSPRING_E2E_KEY_FILE")
	if rpcURL == "" || keyFile == "" {
		t.Fatal("set WELLSPRING_E2E_RPC_URL and WELLSPRING_E2E_KEY_FILE")
	}
	rpc, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	chainID, err := rpc.ChainID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key, err := os.ReadFile(keyFile) //nolint:gosec // test input
	if err != nil {
		t.Fatal(err)
	}
	pk, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(string(key)), "0x"))
	if err != nil {
		t.Fatal(err)
	}
	faucet := crypto.PubkeyToAddress(pk.PublicKey)
	startNonce, err := rpc.NonceAt(ctx, faucet, nil)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "wellspring")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", bin, "../cmd/wellspring").CombinedOutput(); err != nil { //nolint:gosec // bin is a temp path
		t.Fatalf("build: %v\n%s", err, out)
	}
	f := &instance{t: t, bin: bin, env: os.Environ()}
	f.env = append(f.env,
		"WELLSPRING_CAPTCHA_PROVIDER=none",
		"WELLSPRING_IP_LIMIT=1000",
		"WELLSPRING_BATCH_SIZE=5",
		"WELLSPRING_AMOUNT_WEI="+strconv.Itoa(amount),
		"WELLSPRING_RPC_URL="+rpcURL,
		"WELLSPRING_CHAIN_ID="+chainID.String(),
		"WELLSPRING_PRIVATE_KEY_FILE="+keyFile,
		"WELLSPRING_DB_PATH="+filepath.Join(dir, "wellspring.db"),
		"WELLSPRING_LOG_LEVEL=debug",
	)

	recipients := make([]common.Address, claims+1)
	before := make([]*big.Int, len(recipients))
	for i := range recipients {
		recipients[i] = crypto.PubkeyToAddress(newKey(t).PublicKey)
		if before[i], err = rpc.BalanceAt(ctx, recipients[i], nil); err != nil {
			t.Fatal(err)
		}
	}

	f.start()
	id, code := f.claim(recipients[0])
	if code != http.StatusAccepted {
		t.Fatalf("claim: HTTP %d", code)
	}
	c := f.wait(id, "confirmed")
	receipt, err := rpc.TransactionReceipt(ctx, common.HexToHash(c.TxHash))
	if err != nil || receipt.Status != 1 {
		t.Fatalf("receipt of %s: %v %v", c.TxHash, receipt, err)
	}
	if _, code := f.claim(recipients[0]); code != http.StatusTooManyRequests {
		t.Fatalf("repeat claim: HTTP %d, want 429", code)
	}

	ids := make([]string, 0, claims)
	for _, a := range recipients[1:] {
		id, code := f.claim(a)
		if code != http.StatusAccepted {
			t.Fatalf("claim: HTTP %d", code)
		}
		ids = append(ids, id)
	}
	f.poll("a claim sent but not confirmed", func() bool {
		confirmed := 0
		for _, id := range ids {
			switch f.status(id).Status {
			case "sent":
				return true
			case "confirmed":
				confirmed++
			}
		}
		if confirmed == len(ids) {
			f.t.Fatal("every claim confirmed before the kill, so the run did not test recovery")
		}
		return false
	})
	f.kill(syscall.SIGKILL)

	f.start()
	for _, id := range ids {
		f.wait(id, "confirmed")
	}
	if code := f.kill(syscall.SIGTERM); code != 0 {
		t.Fatalf("SIGTERM exit code %d", code)
	}

	nonce, err := rpc.NonceAt(ctx, faucet, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := nonce - startNonce; got != claims+1 {
		t.Errorf("faucet sent %d transactions for %d claims", got, claims+1)
	}
	for i, a := range recipients {
		after, err := rpc.BalanceAt(ctx, a, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := new(big.Int).Sub(after, before[i]); got.Cmp(big.NewInt(amount)) != 0 {
			t.Errorf("%s received %s wei, want %d", a, got, amount)
		}
	}
}

type instance struct {
	t    *testing.T
	bin  string
	env  []string
	base string
	cmd  *exec.Cmd
	log  bytes.Buffer
}

func (f *instance) start() {
	f.t.Helper()
	listen, metrics := freeAddr(f.t), freeAddr(f.t)
	f.base = "http://" + listen
	f.log.Reset()
	f.cmd = exec.CommandContext(f.t.Context(), f.bin) //nolint:gosec // the binary this test built
	f.cmd.Env = append(f.env, "WELLSPRING_LISTEN_ADDR="+listen, "WELLSPRING_METRICS_ADDR="+metrics)
	f.cmd.Stdout, f.cmd.Stderr = &f.log, &f.log
	if err := f.cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	cmd := f.cmd
	f.t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		if f.t.Failed() && cmd == f.cmd {
			f.t.Logf("wellspring output:\n%s", f.log.String())
		}
	})
	f.poll("ready", func() bool {
		resp, err := f.do(http.MethodGet, "/readyz", nil)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
}

// kill signals the process and returns its exit code.
func (f *instance) kill(sig syscall.Signal) int {
	f.t.Helper()
	if err := f.cmd.Process.Signal(sig); err != nil {
		f.t.Fatal(err)
	}
	_ = f.cmd.Wait()
	return f.cmd.ProcessState.ExitCode()
}

func (f *instance) claim(a common.Address) (string, int) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"address": a.Hex()})
	resp, err := f.do(http.MethodPost, "/api/claims", bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var c claimView
	_ = json.NewDecoder(resp.Body).Decode(&c)
	return c.ID, resp.StatusCode
}

type claimView struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	TxHash string `json:"txHash"`
	Error  string `json:"error"`
}

// wait polls claim id until it reaches status.
func (f *instance) wait(id, status string) claimView {
	f.t.Helper()
	var c claimView
	f.poll("claim "+id+" "+status, func() bool {
		c = f.status(id)
		return c.Status == status
	})
	return c
}

// status returns claim id, failing the test if the claim failed.
func (f *instance) status(id string) claimView {
	f.t.Helper()
	var c claimView
	resp, err := f.do(http.MethodGet, "/api/claims/"+id, nil)
	if err != nil {
		return c
	}
	defer func() { _ = resp.Body.Close() }()
	_ = json.NewDecoder(resp.Body).Decode(&c)
	if c.Status == "failed" {
		f.t.Fatalf("claim %s failed: %s", id, c.Error)
	}
	return c
}

func (f *instance) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func (f *instance) poll(what string, done func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for !done() {
		if time.Now().After(deadline) {
			f.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}
