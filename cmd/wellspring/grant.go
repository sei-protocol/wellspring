package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/urfave/cli/v3"
)

// grantCommand requests a grant from a running wellspring and waits for its
// transfer to confirm. Every request names the grant, so rerunning the command
// after a crash or a Job retry resumes the same grant instead of paying again.
func grantCommand() *cli.Command {
	return &cli.Command{
		Name:  "grant",
		Usage: "request a grant from wellspring's grants API and wait for it to confirm",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "url", Usage: "grants API base URL, e.g. http://wellspring-admin.wellspring.svc:8081", Required: true},
			&cli.StringFlag{Name: "token-file", Value: "/var/run/secrets/wellspring/token", Usage: "ServiceAccount token with the wellspring audience, reread on every request", TakesFile: true},
			&cli.StringFlag{Name: "id", Usage: "grant ID; reusing it returns the same grant", Required: true},
			&cli.StringFlag{Name: "address", Usage: "recipient 0x address"},
			&cli.StringFlag{Name: "address-file", Usage: "file holding the recipient 0x address", TakesFile: true},
			&cli.StringFlag{Name: "amount-wei", Usage: "wei to send"},
			&cli.StringFlag{Name: "top-up-to-wei", Usage: "send what brings the recipient's balance to this many wei"},
			&cli.StringFlag{Name: "reason", Usage: "why, recorded in wellspring's audit log", Required: true},
			&cli.DurationFlag{Name: "timeout", Value: 10 * time.Minute, Usage: "give up after this long"},
			&cli.DurationFlag{Name: "poll", Value: 3 * time.Second, Usage: "how often to retry and check the grant"},
		},
		Action: runGrant,
	}
}

func runGrant(ctx context.Context, cmd *cli.Command) error {
	address := cmd.String("address")
	if f := cmd.String("address-file"); f != "" {
		b, err := os.ReadFile(f) //nolint:gosec // the caller sets the path
		if err != nil {
			return fmt.Errorf("read --address-file: %w", err)
		}
		address = strings.TrimSpace(string(b))
	}
	body, err := json.Marshal(map[string]string{
		"id": cmd.String("id"), "address": address, "reason": cmd.String("reason"),
		"amountWei": cmd.String("amount-wei"), "topUpToWei": cmd.String("top-up-to-wei"),
	})
	if err != nil {
		return err
	}
	c := &grantClient{base: strings.TrimSuffix(cmd.String("url"), "/"), tokenFile: cmd.String("token-file"), http: &http.Client{Timeout: 30 * time.Second}}
	ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()
	poll := cmd.Duration("poll")

	method, path, reqBody := http.MethodPost, "/admin/grants", body
	for {
		g, err := c.do(ctx, method, path, reqBody)
		var fatal *fatalError
		switch {
		case errors.As(err, &fatal):
			return err
		case err != nil:
			fmt.Fprintf(os.Stderr, "grant: %v; retrying\n", err)
		case g.Status == "confirmed":
			fmt.Fprintf(os.Stderr, "grant %s confirmed: %s wei to %s, tx %s\n", g.ID, g.AmountWei, g.Address, g.TxHash)
			return nil
		case g.Status == "failed":
			return fmt.Errorf("grant %s failed: %s", g.ID, g.Error)
		default:
			fmt.Fprintf(os.Stderr, "grant %s %s\n", g.ID, g.Status)
			method, path, reqBody = http.MethodGet, "/admin/grants/"+g.ID, nil
		}
		select {
		case <-time.After(poll):
		case <-ctx.Done():
			return fmt.Errorf("grant %s did not confirm: %w", cmd.String("id"), ctx.Err())
		}
	}
}

type grantClient struct {
	base, tokenFile string
	http            *http.Client
}

type grantStatus struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	AmountWei string `json:"amountWei"`
	Status    string `json:"status"`
	TxHash    string `json:"txHash"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

// fatalError is a refusal that retrying the same request cannot change.
type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

func (c *grantClient) do(ctx context.Context, method, path string, body []byte) (*grantStatus, error) {
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, &fatalError{fmt.Sprintf("read --token-file: %v", err)}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, &fatalError{err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var g grantStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&g); err != nil {
		return nil, fmt.Errorf("%s %s: %s", method, path, resp.Status)
	}
	switch {
	case resp.StatusCode < 300:
		return &g, nil
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%s: %s", resp.Status, g.Message)
	default:
		return nil, &fatalError{fmt.Sprintf("%s: %s", resp.Status, g.Message)}
	}
}
