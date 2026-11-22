package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// kubectl runs the user's kubectl, so their kubeconfig and its exec auth
// plugins (aws eks get-token, gke-gcloud-auth-plugin) work as they do for them.
type kubectl struct {
	bin, context, namespace string
}

func (k kubectl) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, k.bin, append([]string{"--context", k.context, "--namespace", k.namespace}, args...)...) //nolint:gosec // the user picks kubectl and its arguments
}

// createToken mints a token for ServiceAccount sa with audience aud.
func (k kubectl) createToken(ctx context.Context, sa, aud string, d time.Duration) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := k.command(ctx, "create", "token", sa, "--audience", aud, "--duration", d.String())
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl create token %s: %w: %s", sa, err, strings.TrimSpace(stderr.String()))
	}
	token := strings.TrimSpace(stdout.String())
	if token == "" {
		return "", fmt.Errorf("kubectl create token %s printed nothing", sa)
	}
	return token, nil
}

var forwarding = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:(\d+) -> `)

// portForward forwards a free local port to port of Service svc and returns
// its URL, and a stop function that ends the forward.
func (k kubectl) portForward(ctx context.Context, svc string, port int) (string, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	var stderr bytes.Buffer
	cmd := k.command(ctx, "port-forward", "--address", "127.0.0.1", "svc/"+svc, ":"+strconv.Itoa(port))
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return "", nil, fmt.Errorf("kubectl port-forward: %w", err)
	}
	stop := func() {
		cancel()
		_ = cmd.Wait()
	}
	local := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if m := forwarding.FindStringSubmatch(sc.Text()); m != nil {
				local <- m[1]
				break
			}
		}
		close(local)
		_, _ = io.Copy(io.Discard, out)
	}()
	select {
	case p, ok := <-local:
		if ok {
			return "http://127.0.0.1:" + p, stop, nil
		}
		stop()
		return "", nil, fmt.Errorf("kubectl port-forward svc/%s: %s", svc, strings.TrimSpace(stderr.String()))
	case <-time.After(30 * time.Second):
		stop()
		return "", nil, errors.New("kubectl port-forward did not start within 30s")
	case <-ctx.Done():
		stop()
		return "", nil, ctx.Err()
	}
}
