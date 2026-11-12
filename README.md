# wellspring

:fountain: SEI testnet faucet for EVM chains, Giga first.

> [!WARNING]
> **Experimental, work in progress.** wellspring is not deployed yet, and its
> API, configuration and storage may change without notice.

wellspring hands out testnet SEI on a Sei EVM chain. One process owns one
signing key and its nonce, so every transfer is signed and sent in order, and a
claim counts as paid only once its receipt succeeds.

It is testnet-only: it refuses to start for Sei mainnet (chain ID 1329).

## Develop

```bash
make verify   # lint, race tests and build, as CI runs them
make fmt      # gofmt and goimports
```

`make e2e` runs the binary against a real node: 31 claims, a SIGKILL mid-batch
and a restart, then checks every recipient was paid exactly once. CI runs it
against anvil. To run it against a local Giga (Autobahn) devnet whose genesis
funds the signer:

```bash
WELLSPRING_E2E_RPC_URL=http://localhost:8545 WELLSPRING_E2E_KEY_FILE="$PWD/key" make e2e
```

## Configuration

Every setting is a flag, and each flag can also be set from a `WELLSPRING_*`
environment variable: `--rpc-url` is `WELLSPRING_RPC_URL`. Run
`wellspring --help` for the full list and defaults.

| Flag | Default | Meaning |
| -- | -- | -- |
| `--rpc-url` | required | EVM JSON-RPC endpoint; must serve `--chain-id` |
| `--chain-id` | required | EVM chain ID to sign for; 1329 (mainnet) is refused |
| `--chain-name` / `--public-rpc-url` | `Sei testnet` / none | What the page shows and offers wallets |
| `--private-key-file` | required | File holding the hex secp256k1 signer key |
| `--amount-wei` | 1 SEI | Amount per claim |
| `--max-recipient-balance-wei` | 10 SEI | Refuse addresses already holding this much |
| `--min-faucet-balance-wei` | 1000 SEI | Below this, `/readyz` fails |
| `--address-cooldown` | `24h` | One claim per address per cooldown |
| `--ip-window` / `--ip-limit` | `24h` / 5 | Claims per client IP per window |
| `--max-queued` | 1000 | Refuse new claims once this many wait |
| `--trust-proxy-hops` | 0 | Reverse proxies in front, for `X-Forwarded-For` |
| `--captcha-provider` | `turnstile` | `turnstile`, `hcaptcha`, or `none` for local use |
| `--captcha-site-key` / `--captcha-secret-file` | required unless `none` | Captcha site key and a file holding its secret |
| `--batch-interval` / `--batch-size` | `500ms` / 100 | How often and how many claims to send |
| `--gas-tip-cap-wei` / `--gas-fee-cap-wei` | 1 / 10 gwei | EIP-1559 fee caps |
| `--confirm-timeout` | `30s` | Wait for a receipt before resyncing the nonce |
| `--db-path` | `wellspring.db` | SQLite database; put it on a persistent volume |
| `--listen-addr` / `--metrics-addr` | `:8080` / `:9090` | HTTP and Prometheus listeners |
| `--log-level` | `info` | JSON logs to stdout at this level |

Secrets are only read from files, such as a mounted Kubernetes secret, so they
never appear in the environment or the pod spec.

## Container

The image is distroless and runs as the `nonroot` user (UID/GID 65532). It
only writes to `/data`, so run it with a read-only root filesystem and a volume
there:

```bash
docker build -t wellspring .
docker run --read-only -v wellspring-data:/data -v ./secrets:/secrets:ro \
  --add-host=host.docker.internal:host-gateway \
  -e WELLSPRING_RPC_URL=http://host.docker.internal:8545 -e WELLSPRING_CHAIN_ID=713715 \
  -e WELLSPRING_PRIVATE_KEY_FILE=/secrets/key -e WELLSPRING_CAPTCHA_PROVIDER=none \
  -p 8080:8080 -p 9090:9090 wellspring
```

On SIGTERM it stops taking claims, stops the sender and closes the database.
Claims it had not settled are reconciled from the chain on the next start. `/healthz` is
liveness; `/readyz` also checks the RPC and the faucet balance.

On Kubernetes, run a single replica with the `Recreate` strategy, since one
signer owns the nonce, and mount a PVC at `/data` with this security context:

```yaml
automountServiceAccountToken: false
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
containers:
  - name: wellspring
    securityContext:
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
```
