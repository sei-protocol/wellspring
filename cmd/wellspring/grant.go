package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/urfave/cli/v3"
)

// grantCommand requests a grant from a running wellspring and waits for its
// transfer to confirm. Every request names the grant, so rerunning the command
// after a crash or a Job retry resumes the same grant instead of paying again.
//
// In-cluster callers pass --url and --token-file. Humans pass --kube-context
// instead: the command then mints a token and port-forwards with their kubectl.
func grantCommand() *cli.Command {
	return &cli.Command{
		Name:  "grant",
		Usage: "request a grant from wellspring's grants API and wait for it to confirm",
		Description: "From a laptop:\n\n" +
			"   wellspring grant --kube-context <ctx> --to 0x… --sei 5000 --reason \"partner onboarding\"\n\n" +
			"From a Job, with a token projected for the wellspring audience:\n\n" +
			"   wellspring grant --url http://wellspring.wellspring.svc:8081 --id <id> --address-file <f> --top-up-to-wei <wei> --reason <why>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "kube-context", Usage: "kubeconfig context of wellspring's cluster; mints a token and port-forwards with kubectl instead of --url and --token-file"},
			&cli.StringFlag{Name: "namespace", Value: "wellspring", Usage: "wellspring's namespace, with --kube-context"},
			&cli.StringFlag{Name: "service", Value: "wellspring", Usage: "wellspring's Service, with --kube-context"},
			&cli.IntFlag{Name: "admin-port", Value: 8081, Usage: "the Service's admin port, with --kube-context"},
			&cli.StringFlag{Name: "service-account", Value: "grants-operator", Usage: "ServiceAccount to mint the token for, with --kube-context"},
			&cli.StringFlag{Name: "audience", Value: "wellspring", Usage: "token audience, with --kube-context"},
			&cli.StringFlag{Name: "kubectl", Value: "kubectl", Usage: "kubectl binary, with --kube-context"},
			&cli.StringFlag{Name: "url", Usage: "grants API base URL, e.g. http://wellspring.wellspring.svc:8081"},
			&cli.StringFlag{Name: "token-file", Value: "/var/run/secrets/wellspring/token", Usage: "ServiceAccount token with the wellspring audience, reread on every request", TakesFile: true},
			&cli.StringFlag{Name: "id", Usage: "grant ID; reusing it returns the same grant. With --kube-context it defaults to <address>-<amount>-<UTC date>"},
			&cli.StringFlag{Name: "address", Aliases: []string{"to"}, Usage: "recipient 0x address"},
			&cli.StringFlag{Name: "address-file", Usage: "file holding the recipient 0x address", TakesFile: true},
			&cli.StringFlag{Name: "sei", Usage: "SEI to send, e.g. 5000 or 0.5"},
			&cli.StringFlag{Name: "top-up-to-sei", Usage: "send what brings the recipient's balance to this many SEI"},
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
	topUp, wei, err := grantAmount(cmd)
	if err != nil {
		return err
	}
	human := cmd.String("kube-context") != ""
	id := cmd.String("id")
	switch {
	case id != "":
	case human:
		if id, err = defaultGrantID(address, topUp, wei, time.Now()); err != nil {
			return err
		}
	default:
		return errors.New("--id is required without --kube-context")
	}
	req := map[string]string{"id": id, "address": address, "reason": cmd.String("reason")}
	if topUp {
		req["topUpToWei"] = wei.String()
	} else {
		req["amountWei"] = wei.String()
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, cmd.Duration("timeout"))
	defer cancel()

	c := &grantClient{base: strings.TrimSuffix(cmd.String("url"), "/"), http: &http.Client{Timeout: 30 * time.Second}}
	if human {
		if cmd.IsSet("url") || cmd.IsSet("token-file") {
			return errors.New("--kube-context replaces --url and --token-file")
		}
		k := kubectl{bin: cmd.String("kubectl"), context: cmd.String("kube-context"), namespace: cmd.String("namespace")}
		// The API server refuses tokens shorter than 10m.
		token, err := k.createToken(ctx, cmd.String("service-account"), cmd.String("audience"), max(10*time.Minute, cmd.Duration("timeout")+time.Minute))
		if err != nil {
			return err
		}
		c.token = func() (string, error) { return token, nil }
		var stop func()
		if c.base, stop, err = k.portForward(ctx, cmd.String("service"), cmd.Int("admin-port")); err != nil {
			return err
		}
		defer stop()
		fmt.Fprintf(os.Stderr, "grant %s: rerunning this command today, or with this --id, resumes it instead of paying again\n", id)
	} else {
		if c.base == "" {
			return errors.New("--url or --kube-context is required")
		}
		file := cmd.String("token-file")
		c.token = func() (string, error) {
			b, err := os.ReadFile(file) //nolint:gosec // the caller sets the path
			if err != nil {
				return "", fmt.Errorf("read --token-file: %w", err)
			}
			return strings.TrimSpace(string(b)), nil
		}
	}
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
			if g.TxHash != "" {
				fmt.Fprintf(os.Stderr, "chain %s (ID %d), RPC %s\n", g.ChainName, g.ChainID, g.RPCURL)
			}
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
			return fmt.Errorf("grant %s did not confirm: %w", id, ctx.Err())
		}
	}
}

// grantAmount reads the one amount flag set, in wei.
func grantAmount(cmd *cli.Command) (topUp bool, wei *big.Int, err error) {
	var set []string
	for _, f := range []string{"sei", "top-up-to-sei", "amount-wei", "top-up-to-wei"} {
		if cmd.String(f) != "" {
			set = append(set, f)
		}
	}
	if len(set) != 1 {
		return false, nil, errors.New("set exactly one of --sei, --top-up-to-sei, --amount-wei and --top-up-to-wei")
	}
	f, v := set[0], cmd.String(set[0])
	if strings.HasSuffix(f, "sei") {
		wei, err = parseSEI(v)
	} else {
		wei, err = parseWei(v)
	}
	if err != nil {
		return false, nil, fmt.Errorf("--%s %q: %w", f, v, err)
	}
	return strings.HasPrefix(f, "top-up-to-"), wei, nil
}

var weiPerSEI = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// parseSEI converts a decimal SEI amount with at most 18 decimals to wei.
func parseSEI(s string) (*big.Int, error) {
	whole, frac, dot := strings.Cut(s, ".")
	if !isDigits(whole) || (dot && !isDigits(frac)) || len(frac) > 18 {
		return nil, errors.New("not a decimal SEI amount with at most 18 decimals")
	}
	wei, _ := new(big.Int).SetString(whole+frac+strings.Repeat("0", 18-len(frac)), 10)
	return wei, nil
}

func parseWei(s string) (*big.Int, error) {
	if !isDigits(s) {
		return nil, errors.New("not a whole number of wei")
	}
	wei, _ := new(big.Int).SetString(s, 10)
	return wei, nil
}

func isDigits(s string) bool {
	if s == "" || len(s) > 78 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// defaultGrantID names a grant after what it sends, to whom and on which UTC
// day, so rerunning the same command that day resumes it rather than paying
// again, while the same grant on a later day is a new one.
func defaultGrantID(address string, topUp bool, wei *big.Int, now time.Time) (string, error) {
	if !common.IsHexAddress(address) {
		return "", fmt.Errorf("--address %q is not a 0x address", address)
	}
	q, r := new(big.Int).QuoRem(wei, weiPerSEI, new(big.Int))
	amount := q.String()
	if r.Sign() != 0 {
		amount += "." + strings.TrimRight(fmt.Sprintf("%018s", r), "0")
	}
	amount += "sei"
	if topUp {
		amount = "topup" + amount
	}
	id := fmt.Sprintf("%s-%s-%s", strings.ToLower(address), amount, now.UTC().Format("20060102"))
	if len(id) > 128 {
		return "", errors.New("the default grant ID is too long; set --id")
	}
	return id, nil
}

type grantClient struct {
	base  string
	token func() (string, error)
	http  *http.Client
}

type grantStatus struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	AmountWei string `json:"amountWei"`
	Status    string `json:"status"`
	ChainID   uint64 `json:"chainId"`
	ChainName string `json:"chainName"`
	RPCURL    string `json:"rpcUrl"`
	TxHash    string `json:"txHash"`
	Error     string `json:"error"`
	Message   string `json:"message"`
}

// fatalError is a refusal that retrying the same request cannot change.
type fatalError struct{ msg string }

func (e *fatalError) Error() string { return e.msg }

func (c *grantClient) do(ctx context.Context, method, path string, body []byte) (*grantStatus, error) {
	token, err := c.token()
	if err != nil {
		return nil, &fatalError{err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, &fatalError{err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
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
