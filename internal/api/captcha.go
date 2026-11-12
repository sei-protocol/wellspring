package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var siteverifyURLs = map[string]string{
	"turnstile": "https://challenges.cloudflare.com/turnstile/v0/siteverify",
	"hcaptcha":  "https://api.hcaptcha.com/siteverify",
}

// Verifier checks a captcha token.
type Verifier interface {
	Verify(ctx context.Context, token, ip string) error
}

// NewVerifier returns the Verifier for provider; "none" accepts every token.
func NewVerifier(provider, secret string) Verifier {
	if provider == "none" {
		return noCaptcha{}
	}
	return siteverify{url: siteverifyURLs[provider], secret: secret, client: &http.Client{Timeout: 10 * time.Second}}
}

type noCaptcha struct{}

func (noCaptcha) Verify(context.Context, string, string) error { return nil }

type siteverify struct {
	url    string
	secret string
	client *http.Client
}

var errCaptcha = errors.New("captcha verification failed")

func (v siteverify) Verify(ctx context.Context, token, ip string) error {
	if token == "" {
		return errCaptcha
	}
	form := url.Values{"secret": {v.secret}, "response": {token}, "remoteip": {ip}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.url, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return err
	}
	if !out.Success {
		return errCaptcha
	}
	return nil
}
