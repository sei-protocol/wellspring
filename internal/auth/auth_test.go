package auth

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeAPI answers TokenReviews: a token maps to the review status it gets.
func fakeAPI(t *testing.T, statuses map[string]string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/authentication.k8s.io/v1/tokenreviews" || r.Header.Get("Authorization") != "Bearer reviewer" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var review tokenReview
		_ = json.NewDecoder(r.Body).Decode(&review)
		if len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != "wellspring" {
			t.Errorf("review asked for audiences %v", review.Spec.Audiences)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":` + statuses[review.Spec.Token] + `}`))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), ca, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("reviewer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return srv, dir
}

func sa(user string, audiences ...string) string {
	b, _ := json.Marshal(map[string]any{"authenticated": true, "audiences": audiences, "user": map[string]string{"username": user}})
	return string(b)
}

func TestAuthenticate(t *testing.T) {
	srv, dir := fakeAPI(t, map[string]string{
		"exact":     sa("system:serviceaccount:wellspring:grants-operator", "wellspring"),
		"second":    sa("system:serviceaccount:giga-testnet-1:harpoon-setup", "wellspring"),
		"same-ns":   sa("system:serviceaccount:giga-testnet-1:validator", "wellspring"),
		"star":      sa("system:serviceaccount:giga-testnet-2:*", "wellspring"),
		"other-sa":  sa("system:serviceaccount:wellspring:default", "wellspring"),
		"other-aud": sa("system:serviceaccount:wellspring:grants-operator", "https://kubernetes.default.svc"),
		"user":      sa("alice@example.com", "wellspring"),
		"invalid":   `{"authenticated":false,"error":"token expired"}`,
		"unvouched": `{"authenticated":false,"audiences":["wellspring"],"user":{"username":"system:serviceaccount:wellspring:grants-operator"}}`,
		"no-name":   sa("system:serviceaccount:wellspring", "wellspring"),
		"other-ns":  sa("system:serviceaccount:giga-testnet-10:harpoon-setup", "wellspring"),
	})
	r, err := NewReviewer(srv.URL, filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt"), "wellspring",
		[]string{"wellspring/grants-operator", "giga-testnet-1/harpoon-setup"})
	if err != nil {
		t.Fatal(err)
	}
	for token, want := range map[string]error{
		"exact": nil, "second": nil,
		"other-sa": ErrForbidden, "same-ns": ErrForbidden, "other-ns": ErrForbidden, "star": ErrForbidden,
		"other-aud": ErrUnauthenticated, "user": ErrUnauthenticated, "invalid": ErrUnauthenticated,
		"unvouched": ErrUnauthenticated, "no-name": ErrUnauthenticated, "": ErrUnauthenticated,
		string(make([]byte, maxTokenBytes+1)): ErrUnauthenticated,
	} {
		if _, err := r.Authenticate(context.Background(), token); !errors.Is(err, want) {
			t.Errorf("token %.12q: got %v, want %v", token, err, want)
		}
	}
	if who, _ := r.Authenticate(context.Background(), "second"); who != "giga-testnet-1/harpoon-setup" {
		t.Fatalf("caller %q", who)
	}
}

func TestAuthenticateFailsClosed(t *testing.T) {
	srv, dir := fakeAPI(t, nil)
	r, err := NewReviewer(srv.URL, filepath.Join(dir, "missing"), filepath.Join(dir, "ca.crt"), "wellspring", []string{"a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if who, err := r.Authenticate(context.Background(), "t"); err == nil || who != "" {
		t.Fatal("authenticated without reading its own token")
	}
	untrusted, err := NewReviewer(srv.URL, filepath.Join(dir, "token"), "", "wellspring", []string{"a/b"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Authenticate(context.Background(), "t"); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("API with an untrusted certificate: %v", err)
	}
}
