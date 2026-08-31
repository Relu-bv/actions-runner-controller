# Local capacity gating (Relu fork)

> **This is a fork-only feature.** It does not exist upstream in
> `actions/actions-runner-controller`. Everything it adds lives under
> `cmd/ghalistener/localcapacity/` plus ~30 lines of wiring in
> `cmd/ghalistener/main.go`, deliberately, so rebasing onto upstream stays cheap.

Make a runner scale set burst to the cloud **only when a pool of static,
non-ARC runners is fully busy** — typically on-premises machines you have
already paid for.

## Why this works

GitHub does not implement runner priority; requests for it
([actions/runner#1665](https://github.com/actions/runner/issues/1665),
[community#25309](https://github.com/orgs/community/discussions/25309)) are
still open, and the old deterministic repo → org → enterprise search order was
[removed in 2022](https://github.blog/changelog/2022-01-04-github-actions-changing-the-search-order-for-self-hosted-runners/).

But there is a supported mechanism that gets the same result: **advertised
capacity**. The listener already sends its max runners on every long poll:

```go
msg, err := l.client.GetMessage(ctx, lastMessageID, int(l.maxRunners.Load()))
```

Since [#3431](https://github.com/actions/actions-runner-controller/pull/3431)
the Actions service honours that number. From the maintainer closing
[#3446](https://github.com/actions/actions-runner-controller/issues/3446):

> Closing this one since it is fixed on the back-end side. All versions after
> `0.9.2` contain the capacity information, and the back-end would not assign
> jobs to the scale set anymore if the capacity is 0.

So while we report **0**, the backend does not assign jobs to this scale set;
they stay queued and any other online, idle, label-matching runner picks them
up. That is precisely the local pool.

`actions/scaleset` already exposes the knob and documents it as concurrency
safe:

```go
// SetMaxRunners sets the capacity of the scaleset. It is concurrently
// safe to update the max runners during listener.Run.
func (l *Listener) SetMaxRunners(count int)
```

Upstream only ever calls it once, at construction. This fork adds a poller that
keeps calling it.

**Draining is free.** Scale set runners are ephemeral — one job each, then they
exit. Closing the gate stops *new* runners being created; in-flight jobs finish
normally and nothing is evicted.

## Configuration

Environment variables on the **listener** container. No CRD, controller, or
chart changes — the stock chart already supports `listenerTemplate`.

| Variable | Default | Meaning |
|---|---|---|
| `LOCAL_CAPACITY_ENABLED` | `false` | Master switch. Off = stock upstream behaviour. |
| `LOCAL_CAPACITY_LABELS` | *(required)* | Comma-separated labels a runner must carry **all** of to count as local capacity. Case-insensitive. |
| `LOCAL_CAPACITY_POLL_INTERVAL` | `15s` | Refresh interval. Minimum `5s`. |
| `LOCAL_CAPACITY_IDLE_THRESHOLD` | `0` | Idle local runners tolerated while the gate stays **open**. `0` = no cloud runners while any local runner is idle. |
| `LOCAL_CAPACITY_EXCLUDE_NAME_PREFIXES` | *(scale set name)* | Extra runner-name prefixes to ignore. |
| `LOCAL_CAPACITY_FAILURE_THRESHOLD` | `3` | Consecutive poll failures before failing **open**. |

Example, in `gha-runner-scale-set` values:

```yaml
maxRunners: 20
minRunners: 0   # see the warning below

listenerTemplate:
  spec:
    containers:
      - name: listener
        env:
          - name: LOCAL_CAPACITY_ENABLED
            value: "true"
          - name: LOCAL_CAPACITY_LABELS
            value: "self-hosted,linux-x64"
          - name: LOCAL_CAPACITY_POLL_INTERVAL
            value: "15s"
```

## Behaviour

With 5 local runners, `maxRunners: 20`, and a burst of 50 jobs:

1. 5 local runners idle → gate **closed** (capacity `0`). The backend assigns
   the first 5 jobs to the local pool.
2. Local pool saturated → on the next tick the gate **opens** (capacity `20`)
   and EKS absorbs the overflow.
3. Local runners free up → gate **closes** again. Existing cloud runners finish
   their current job and exit; no new ones are created.

## Things you must know

**`minRunners` must be `0`.** Warm runners are already registered and idle, so
the backend can hand them a job without consulting advertised capacity. The gate
cannot hold them back. The listener logs a warning if you set both.

**Overflow is delayed by up to one poll interval.** The gate is binary by
design. A proportional version (`maxRunners - idleLocal`) would let the backend
assign work while local runners sat idle — the exact thing this exists to
prevent — so the trade is one tick of latency at the start of a burst.

**Enterprise scope requires a classic PAT.** The enterprise runners API rejects
GitHub App credentials; it needs `manage_runners:enterprise`. Org and repo
scopes accept either. With a GitHub App, the app **id** is required — a client
ID (`Iv23li...`) will not work for this REST path.

**It fails open.** After `LOCAL_CAPACITY_FAILURE_THRESHOLD` consecutive polling
errors the gate restores full capacity. A closed gate with a broken poller would
stall CI indefinitely and look exactly like a GitHub outage; running jobs in the
cloud only costs money.

**Rate limits.** Each tick is one paginated call to the runners API. A classic
PAT gets 5,000 requests/hour, a GitHub App installation 15,000. `15s` polling is
~240 calls/hour, comfortable on either, but do not lower it without doing the
arithmetic.

**Self-exclusion is load bearing.** The runners API returns this scale set's own
ephemeral runners, and they carry the same labels by construction. Counting them
would deadlock the gate: our idle runner closes the gate → no work is assigned →
it stays idle. The listener seeds the exclusion list with the ephemeral runner
set and scale set names automatically.

**The routing behaviour is server-side and undocumented.** GitHub documents that
a zero-capacity scale set is not assigned jobs; that a *static* runner then
picks them up is the logical consequence but is not written down anywhere.
**Verify it in your environment before relying on it** (see below), and be aware
GitHub could change it.

## Verifying before you trust it

No code needed — this works on stock upstream ARC:

1. Deploy a scale set with `maxRunners: 0` and a label your local runners also
   carry.
2. Queue a job with `runs-on:` that label.
3. Watch where it lands.

A local runner picking it up confirms the mechanism. The job sitting queued
means the backend reserved it for the scale set anyway, and this feature cannot
work — stop here.

## Maintaining the fork

- Everything is additive; no upstream file is modified except
  `cmd/ghalistener/main.go`.
- `go test ./cmd/ghalistener/...` covers the gate policy, fail-open behaviour,
  self-exclusion, and config parsing.
- On rebase, re-check `listener.SetMaxRunners` still exists with the same
  concurrency guarantee, and that `GetMessage` still receives max runners.
