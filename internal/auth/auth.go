// Package auth authenticates grant callers by their Kubernetes ServiceAccount
// tokens, which the Kubernetes API reviews, so no shared secret exists.
package auth

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

// Errors Authenticate returns for a refused caller.
var (
	ErrUnauthenticated = errors.New("token is not a valid ServiceAccount token for this audience")
	ErrForbidden       = errors.New("ServiceAccount may not make grants")
)

// maxTokenBytes bounds the tokens sent for review; ServiceAccount tokens are
// about a kilobyte.
const maxTokenBytes = 8 << 10

const saPrefix = "system:serviceaccount:"

// Reviewer authenticates callers through the Kubernetes TokenReview API and
// authorises them against an allowlist.
type Reviewer struct {
	url       string
	tokenFile string
	audience  string
	allow     []string
	client    *http.Client
}

// NewReviewer returns a Reviewer for the Kubernetes API at apiURL, which it
// calls with the token in tokenFile, read on every review so a rotated token is
// picked up. caFile, when non-empty, holds the CA bundle of the API. allow
// holds namespace/name or namespace/* entries.
func NewReviewer(apiURL, tokenFile, caFile, audience string, allow []string) (*Reviewer, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // the operator sets the path
		if err != nil {
			return nil, fmt.Errorf("read Kubernetes CA: %w", err)
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(pem) {
			return nil, errors.New("kubernetes CA file holds no certificates")
		}
	}
	return &Reviewer{
		url:       strings.TrimSuffix(apiURL, "/") + "/apis/authentication.k8s.io/v1/tokenreviews",
		tokenFile: tokenFile,
		audience:  audience,
		allow:     allow,
		client:    &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}},
	}, nil
}

type tokenReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Token     string   `json:"token"`
		Audiences []string `json:"audiences"`
	} `json:"spec"`
	Status struct {
		Authenticated bool     `json:"authenticated"`
		Audiences     []string `json:"audiences"`
		User          struct {
			Username string `json:"username"`
		} `json:"user"`
	} `json:"status"`
}

// Authenticate returns the namespace/name of the ServiceAccount token belongs
// to. It fails with ErrUnauthenticated unless the Kubernetes API vouches for
// token with the configured audience, and with ErrForbidden unless the
// ServiceAccount is allowed. Any other error means the review did not happen.
func (r *Reviewer) Authenticate(ctx context.Context, token string) (string, error) {
	if token == "" || len(token) > maxTokenBytes {
		return "", ErrUnauthenticated
	}
	own, err := os.ReadFile(r.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read Kubernetes token: %w", err)
	}
	var review tokenReview
	review.APIVersion, review.Kind = "authentication.k8s.io/v1", "TokenReview"
	review.Spec.Token, review.Spec.Audiences = token, []string{r.audience}
	body, err := json.Marshal(review)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(own)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token review: Kubernetes API answered %s", resp.Status)
	}
	review = tokenReview{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&review); err != nil {
		return "", fmt.Errorf("token review: %w", err)
	}
	st := review.Status
	if !st.Authenticated || !slices.Contains(st.Audiences, r.audience) {
		return "", ErrUnauthenticated
	}
	ns, name, ok := strings.Cut(strings.TrimPrefix(st.User.Username, saPrefix), ":")
	if !strings.HasPrefix(st.User.Username, saPrefix) || !ok || ns == "" || name == "" {
		return "", ErrUnauthenticated
	}
	if !slices.Contains(r.allow, ns+"/"+name) && !slices.Contains(r.allow, ns+"/*") {
		return "", ErrForbidden
	}
	return ns + "/" + name, nil
}
