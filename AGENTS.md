# wellspring

Testnet faucet for Sei EVM chains. Run `make verify` before pushing; it mirrors CI.

## Invariants

- **One signer, one nonce owner.** Giga's mempool admits a sender's transaction
  only at exactly its next nonce, and `eth_getTransactionCount` reads committed
  state only. One goroutine signs and sends every transfer in nonce order; never
  add a second sender for the same key or run more than one replica.
- **Persist before broadcast.** A signed transaction is written to the store
  before it is sent, so a restart can find and reconcile it.
- **The receipt decides.** Giga's EVM-only `CheckTx` is stateless, so a broadcast
  that succeeds can still be rejected in the block. A claim is confirmed only by
  a successful receipt.
- **Testnet only.** Never remove the mainnet (1329) refusal.
- **No secrets in logs or errors.** The signer key and captcha secret never
  appear in output, including config errors.

## Conventions

- Go from `go.mod`; golangci-lint pinned in the Makefile and CI (`check-lint-pin`).
- GitHub Actions pinned by commit SHA with a `# vX.Y.Z` comment, and
  `persist-credentials: false` on every checkout.
