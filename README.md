# wellspring

:fountain: SEI testnet faucet for EVM chains, Giga first.

wellspring hands out testnet SEI on a Sei EVM chain. One process owns one
signing key and its nonce, so every transfer is signed and sent in order, and a
claim counts as paid only once its receipt succeeds.

It is testnet-only: it refuses to start for Sei mainnet (chain ID 1329).

## Develop

```bash
make verify   # lint, race tests and build, as CI runs them
make fmt      # gofmt and goimports
```

## Configuration

Everything is read from the environment.

| Variable | Default | Meaning |
| -- | -- | -- |
| `RPC_URL` | required | EVM JSON-RPC endpoint; must serve `CHAIN_ID` |
| `CHAIN_ID` | required | EVM chain ID to sign for; 1329 is refused |
| `PRIVATE_KEY_FILE` / `PRIVATE_KEY` | required, one of | Hex secp256k1 signer key, as a mounted file or a value |
| `AMOUNT_WEI` | 1 SEI | Amount per claim |
| `MAX_RECIPIENT_BALANCE_WEI` | 10 SEI | Refuse addresses already holding this much |
| `MIN_FAUCET_BALANCE_WEI` | 1000 SEI | Below this, `/readyz` fails |
| `ADDRESS_COOLDOWN` | `24h` | One claim per address per cooldown |
| `IP_WINDOW` / `IP_LIMIT` | `24h` / 5 | Claims per client IP per window |
| `MAX_QUEUED` | 1000 | Refuse new claims once this many wait |
| `TRUST_PROXY_HOPS` | 0 | Reverse proxies in front, for `X-Forwarded-For` |
| `CAPTCHA_PROVIDER` | `turnstile` | `turnstile`, `hcaptcha`, or `none` for local use |
| `CAPTCHA_SITE_KEY` / `CAPTCHA_SECRET` | required unless `none` | Captcha keys |
| `BATCH_INTERVAL` / `BATCH_SIZE` | `500ms` / 100 | How often and how many claims to send |
| `GAS_TIP_CAP_WEI` / `GAS_FEE_CAP_WEI` | 1 / 10 gwei | EIP-1559 fee caps |
| `CONFIRM_TIMEOUT` | `30s` | Wait for a receipt before resyncing the nonce |
| `DB_PATH` | `wellspring.db` | SQLite database |
| `LISTEN_ADDR` / `METRICS_ADDR` | `:8080` / `:9090` | HTTP and Prometheus listeners |
