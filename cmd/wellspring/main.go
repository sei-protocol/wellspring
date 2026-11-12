// Command wellspring runs the testnet faucet.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfave/cli/v3"

	"github.com/sei-protocol/wellspring/internal/config"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cmd := &cli.Command{
		Name:  "wellspring",
		Usage: "testnet SEI faucet for Sei EVM chains",
		Flags: config.Flags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			cfg, err := config.FromCommand(cmd)
			if err != nil {
				return err
			}
			slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))
			slog.Info("config valid", "chain_id", cfg.ChainID, "faucet", crypto.PubkeyToAddress(cfg.PrivateKey.PublicKey))
			return nil
		},
	}
	if err := cmd.Run(ctx, os.Args); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("wellspring failed", "err", err)
		os.Exit(1)
	}
}
