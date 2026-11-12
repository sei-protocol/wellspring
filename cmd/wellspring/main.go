// Command wellspring runs the testnet faucet.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v3"

	"github.com/sei-protocol/wellspring/internal/api"
	"github.com/sei-protocol/wellspring/internal/config"
	"github.com/sei-protocol/wellspring/internal/sender"
	"github.com/sei-protocol/wellspring/internal/store"
)

const (
	balanceInterval = 15 * time.Second
	nonceSettle     = 2 * time.Second
	shutdownTimeout = 20 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cmd := &cli.Command{
		Name:  "wellspring",
		Usage: "testnet SEI faucet for Sei EVM chains",
		Flags: config.Flags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			cfg, err := config.FromCommand(cmd)
			if err != nil {
				return err
			}
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))
			return run(ctx, cfg)
		},
	}
	if err := cmd.Run(ctx, os.Args); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("wellspring failed", "err", err)
		os.Exit(1)
	}
}

// run serves until ctx ends, then stops taking claims before it stops the
// sender. Claims the sender had not settled are reconciled on the next start.
func run(ctx context.Context, cfg *config.Config) error {
	client, err := ethclient.DialContext(ctx, cfg.RPCURL)
	if err != nil {
		return fmt.Errorf("dial RPC: %w", err)
	}
	defer client.Close()
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("read chain ID: %w", err)
	}
	if !chainID.IsUint64() || chainID.Uint64() != cfg.ChainID {
		return fmt.Errorf("RPC serves chain %s, not --chain-id %d", chainID, cfg.ChainID)
	}

	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = st.Close() }()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	snd := sender.New(sender.Config{
		ChainID:        new(big.Int).SetUint64(cfg.ChainID),
		Key:            cfg.PrivateKey,
		GasTipCap:      cfg.GasTipCap,
		GasFeeCap:      cfg.GasFeeCap,
		BatchSize:      cfg.BatchSize,
		Interval:       cfg.BatchInterval,
		ConfirmTimeout: cfg.ConfirmTimeout,
		Settle:         nonceSettle,
	}, client, st, reg)
	srv := api.New(cfg, st, client, snd, api.NewVerifier(cfg.CaptchaProvider, cfg.CaptchaSecret), reg)

	servers := []*http.Server{
		newServer(cfg.ListenAddr, srv.Handler()),
		newServer(cfg.MetricsAddr, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})),
	}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("serve %s: %w", s.Addr, err)
			}
		}()
	}

	workCtx, stopWork := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Go(func() { _ = snd.Run(workCtx) })
	wg.Go(func() { _ = snd.WatchBalance(workCtx, balanceInterval) })
	slog.Info("wellspring started", "chain_id", cfg.ChainID, "faucet", snd.Address(), "listen", cfg.ListenAddr)

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
	case err = <-errc:
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	stopWork()
	wg.Wait()
	return err
}

func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
}
