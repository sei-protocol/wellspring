# wellspring runbook

The faucet is one pod, `<release>-wellspring-0`, holding one signer key. Its
claims live in SQLite at `/data`. Useful commands, with `NS` the namespace:

```bash
kubectl -n $NS logs sts/<release>-wellspring          # JSON logs
kubectl -n $NS port-forward sts/<release>-wellspring 8080 9090
curl -s localhost:8080/api/info                        # faucet address and balance
curl -s localhost:8080/readyz                          # why it is not ready
curl -s localhost:9090/metrics | grep ^wellspring_
cast nonce --block latest $FAUCET --rpc-url $RPC       # compare with pending
cast nonce --block pending $FAUCET --rpc-url $RPC
```

Never run a second copy of the faucet with the same key, and never force-delete
its pod: two senders sign the same nonces and pay claims twice.

## WellspringDown

Prometheus cannot scrape the pod. Check `kubectl -n $NS get pod`, then the
pod's events and its last logs (`logs --previous`). A pod that will not start
usually logs the reason on its first line: a missing secret file, an
unreachable RPC, or a chain ID that does not match `--chain-id`.

## WellspringNotReady

`/readyz` fails when the RPC does not answer, when the faucet balance is below
`--min-faucet-balance-wei`, or when the balance has not been read recently.
`curl /readyz` says which. For the RPC, check the node the faucet points at;
for the balance, see WellspringBalanceLow. The page refuses claims while it is
not ready.

## WellspringBalanceLow

Send SEI to the faucet address from `/api/info`. On an EVM-only chain the
only source of SEI is the genesis `evm_alloc`, so top it up from whichever
account the alloc funded for that purpose. If claims drain it faster than
expected, look at `wellspring_claim_requests_total` by outcome before lowering
`config.amount-wei` or `config.ip-limit`.

## WellspringSenderStuck

Claims are queued but none is finishing. In the logs, look for send or receipt
errors. Compare the pending and latest nonces:

- **Pending is above latest and not moving:** the node holds the faucet's
  transactions but is not including them. Check that the chain is producing
  blocks. The sender has no same-nonce fee bump yet, so a transaction priced
  below what the chain now accepts stays stuck until the chain accepts it.
- **Pending equals latest:** the node dropped the transactions. Restart the
  pod with `kubectl -n $NS rollout restart sts/<release>-wellspring`: on start
  it reconciles every unsettled claim against the chain and rebroadcasts the
  signed ones that did not land.
- **The log says "faucet nonce spent by a transaction it did not send":**
  something else is signing with the faucet key. Stop the faucet
  (`kubectl -n $NS scale sts/<release>-wellspring --replicas=0`) and rotate
  the key.

## WellspringSendErrors

The node refuses broadcasts, or the sender keeps resyncing its nonce. The
logs give the node's reason. Common ones: the fee caps are below the node's
minimum gas price (raise `config.gas-fee-cap-wei` and `config.gas-tip-cap-wei`),
or the faucet balance cannot cover a transfer.

## WellspringRPCErrors

RPC reads are failing. Check the node behind `config.rpc-url` and the
NetworkPolicy egress rules, if set. The sender retries on its own.

## Rotating the signer key

1. Generate a key (`cast wallet new`) and fund its address.
2. Wait for `wellspring_queue_depth` to reach 0, then scale the StatefulSet
   to 0 replicas.
3. Replace the key in the signer secret, then scale back to 1.
4. Move what is left on the old address to the new one, and stop using the
   old key.

Claims that were signed but not settled when the faucet stopped are
rebroadcast with the old key's signature on the next start, so finish step 4
only after they settle.

## Restoring the database

The database only holds rate limits and claim history; funds and nonces live
on the chain. Restore `/data` from a volume snapshot with the pod scaled to 0.
Without a backup, start from an empty volume: the sender reads its nonce from
the chain, previous claimants can claim again at once, and unsettled claims
are lost and must be claimed again.
