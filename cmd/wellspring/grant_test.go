package main

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSEI(t *testing.T) {
	for in, want := range map[string]string{
		"5000":                 "5000000000000000000000",
		"0.5":                  "500000000000000000",
		"1.000000000000000001": "1000000000000000001",
		"0":                    "0",
	} {
		if got, err := parseSEI(in); err != nil || got.String() != want {
			t.Errorf("parseSEI(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", ".5", "5.", "-1", "1e3", "0x10", "1.0000000000000000001", "1,000"} {
		if got, err := parseSEI(in); err == nil {
			t.Errorf("parseSEI(%q) = %v; want an error", in, got)
		}
	}
}

func TestDefaultGrantID(t *testing.T) {
	day := time.Date(2026, 11, 21, 23, 30, 0, 0, time.FixedZone("", -5*3600))
	addr := "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"
	sei, _ := parseSEI("5000")
	for _, tc := range []struct {
		topUp bool
		wei   *big.Int
		want  string
	}{
		{false, sei, "0xabcdef0123456789abcdef0123456789abcdef01-5000sei-20261122"},
		{true, sei, "0xabcdef0123456789abcdef0123456789abcdef01-topup5000sei-20261122"},
		{false, big.NewInt(1), "0xabcdef0123456789abcdef0123456789abcdef01-0.000000000000000001sei-20261122"},
	} {
		if got, err := defaultGrantID(addr, tc.topUp, tc.wei, day); err != nil || got != tc.want {
			t.Errorf("defaultGrantID(%v, %v) = %q, %v; want %q", tc.topUp, tc.wei, got, err, tc.want)
		}
	}
	if _, err := defaultGrantID("0x1234", false, sei, day); err == nil {
		t.Error("defaultGrantID accepted a short address")
	}
}

// TestGrantWithKubeContext runs the human flow against a fake kubectl that
// mints a token and "forwards" to a fake grants API.
func TestGrantWithKubeContext(t *testing.T) {
	var posted map[string]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer minted-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthenticated"}`))
			return
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&posted)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"` + posted["id"] + `","status":"queued"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"x","status":"confirmed","txHash":"0xabc","chainId":1330}`))
	}))
	defer api.Close()
	u, _ := url.Parse(api.URL)

	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "kubectl")
	script := `#!/bin/sh
echo "$@" >> ` + argsLog + `
case "$*" in
*"create token"*) echo minted-token ;;
*port-forward*) echo "Forwarding from 127.0.0.1:` + u.Port() + ` -> 8081"; exec sleep 60 ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil { //nolint:gosec // an executable test fixture
		t.Fatal(err)
	}
	to := "0x00000000000000000000000000000000000000aA"
	err := grantCommand().Run(context.Background(), []string{"grant",
		"--kubectl", fake, "--kube-context", "prod", "--to", to, "--sei", "2.5",
		"--reason", "partner", "--poll", "10ms", "--timeout", "30s"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"id":        strings.ToLower(to) + "-2.5sei-" + time.Now().UTC().Format("20060102"),
		"address":   to,
		"amountWei": "2500000000000000000",
		"reason":    "partner",
	}
	for k, v := range want {
		if posted[k] != v {
			t.Errorf("posted %s = %q; want %q", k, posted[k], v)
		}
	}
	b, _ := os.ReadFile(argsLog) //nolint:gosec // the test's own file
	for _, args := range []string{
		"--context prod --namespace wellspring create token grants-operator --audience wellspring --duration 10m0s",
		"--context prod --namespace wellspring port-forward --address 127.0.0.1 svc/wellspring :8081",
	} {
		if !strings.Contains(string(b), args) {
			t.Errorf("kubectl was not run with %q; ran:\n%s", args, b)
		}
	}
}

func TestGrantRefusesBadFlags(t *testing.T) {
	to := "0x00000000000000000000000000000000000000aA"
	for name, args := range map[string][]string{
		"no id in-cluster":      {"--url", "http://x", "--to", to, "--sei", "1"},
		"two amounts":           {"--kube-context", "c", "--to", to, "--sei", "1", "--amount-wei", "1"},
		"no amount":             {"--kube-context", "c", "--to", to},
		"bad wei":               {"--url", "http://x", "--id", "a", "--to", to, "--amount-wei", "1.5"},
		"url with kube-context": {"--kube-context", "c", "--url", "http://x", "--to", to, "--sei", "1"},
		"no url":                {"--id", "a", "--to", to, "--sei", "1"},
	} {
		args = append([]string{"grant", "--reason", "r", "--kubectl", "/nonexistent"}, args...)
		if err := grantCommand().Run(context.Background(), args); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
