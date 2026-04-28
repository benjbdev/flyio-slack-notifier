# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

The Makefile pins `GO ?= /usr/local/go/bin/go` (override with `make GO=go ...` if your Go is on PATH).

- `make dev` — run from source against `config.yaml` (`./cmd/notifier`)
- `make build` — produce `./notifier`; `make run` builds then runs with `--config $(CONFIG)`
- `make test` — full unit suite (hermetic; no Fly/Slack network)
- `make fmt` / `make vet` / `make tidy` — standard Go hygiene
- `make clean` — removes the binary **and** `notifier.db`

Single test / package: `go test ./internal/poller -run TestBootstrapSuppresses` (replace package and `-run` regex).

Runtime needs `FLY_API_TOKEN` and `SLACK_WEBHOOK_FLY_NOTIF` in env or `.env`. `notifier.db` (BoltDB) holds the event cursor — delete it to fully reset state and force a fresh bootstrap.

## Architecture

One process, two producers feeding one consumer over a buffered `chan event.Event` (size 256, see `cmd/notifier/main.go`):

```
poller  ─┐
         ├─► chan event.Event ──► slack.Dispatcher ──► webhook
digester ┘
```

- **`internal/poller`** ticks every `poll_interval` (default 30s). For each app it calls `flyapi.ListMachines`, then walks each machine's `events[]` array. The `Store` (BoltDB, two buckets: `event_cursors` keyed `app/machineID` → highest unix-ms timestamp seen; `meta` keyed `app/name` → strings) is the high-watermark; events with `Timestamp <= cursor` are skipped. `mapMachineEvent` translates Fly's `(type, status)` strings into `event.Kind` + severity; unknown combinations fall through to a generic `KindMachineEvent` rather than being dropped.
- **Bootstrap** (`Poller.bootstrap=true` on first pass only): cursors are advanced but no events are emitted. This is why a fresh start is silent — delete `notifier.db` to replay from "now".
- **Deploy detection** is separate from machine events: `detectDeploy` computes the dominant `image_ref` across an app's machines and compares against `meta["image_ref"]`. A change emits a single `KindDeploy` event, not one per machine.
- **`internal/digest`** is a cron-driven snapshotter (UTC, `robfig/cron/v3`). It calls the same `flyapi.Client.ListMachines` independently of the poller, summarizes per-app state/region/check counts into a `Snapshot`, and emits a single `KindDigest` event carrying the snapshot in `Event.Payload`. Acts as the heartbeat — if digests stop arriving, the notifier or its connectivity is broken.
- **`internal/slack`** is the only consumer. `Dispatcher.handle` does in-memory dedup (sha256 over kind+app+machine+region+sorted fields, default 5-minute window), formats via `FormatEvent` → Block Kit JSON, and POSTs with retry on 429/5xx (honors `Retry-After`, exponential backoff capped at 30s, max 4 retries). **Digest events bypass dedup** so the recurring summary doesn't collapse to one message.
- **`internal/event`** defines the normalized `Event` struct that decouples producers from the dispatcher. `Fields map[string]string` is rendered into Slack section fields in a stable priority order (see `orderedFieldBlocks`); Slack caps at 10 fields per section so the priority list matters.
- **`internal/config`** loads YAML, expands `${VAR}` from env (after `.env` is loaded — `LoadDotenv` does **not** override already-set env vars), applies defaults, validates. `Duration` is a custom type wrapping `time.Duration` with YAML unmarshaling.

### Things that look like bugs but aren't

- The poller's first call inside `Run` is `pollAll` *before* the ticker — this is the bootstrap pass. `bootstrap` flips to `false` immediately after.
- `slack.Dispatcher.dedup` is unbounded only in pathological cases; expired entries are purged opportunistically on each `isDuplicate` call.
- `slack.SlackConfig.Routing` is parsed but unused — reserved for future per-app channel routing.
- The event channel uses non-blocking `select` (`p.emit`); if it fills up (256 buffered), events are dropped with a warning rather than blocking the poll loop.

## Conventions

- Module path: `github.com/benjbdev/flyio-slack-notifier`. Use this prefix for new internal imports.
- All packages use `slog` with a `component` attribute; new components should follow (`logger.With("component", "foo")`).
- Cron schedules are **UTC** (set explicitly in `main.go`), not local time.
- Tests are colocated (`*_test.go`) and hermetic — they fake the Fly API with `httptest` and use temp BoltDB files. Don't introduce real network calls in unit tests.
