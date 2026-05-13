# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

The Makefile pins `GO ?= /usr/local/go/bin/go` (override with `make GO=go ...` if your Go is on PATH).

- `make dev` — run from source against `config.yaml` (`./cmd/notifier`)
- `make build` — produce `./notifier`; `make run` builds then runs with `--config $(CONFIG)`
- `make test` — full unit suite (hermetic; no Fly/Slack network)
- `make fmt` / `make vet` / `make tidy` — standard Go hygiene
- `make clean` — removes the binary **and** `notifier.db`

Single test / package: `go test ./internal/poller -run TestBootstrapSuppresses` (replace package and `-run` regex). Run `go test -race ./...` after touching `Poller.pollAll` or `Digester.snapshot` — both fan out per-app HTTP calls across goroutines.

Runtime needs `FLY_API_TOKEN` and `SLACK_WEBHOOK_FLY_NOTIF` in env or `.env`. `notifier.db` (BoltDB) holds the event cursor — delete it to fully reset state and force a fresh bootstrap.

## Architecture

One process, two producers feeding one consumer over a buffered `chan event.Event` (size 256, see `cmd/notifier/main.go`):

```
poller  ─┐
         ├─► chan event.Event ──► slack.Dispatcher ──► webhook
digester ┘
```

- **`internal/poller`** ticks every `poll_interval` (default 30s). For each app it calls `flyapi.ListMachines`, then walks each machine's `events[]` array. The `Store` (BoltDB, two buckets: `event_cursors` keyed `app/machineID` → highest unix-ms timestamp seen; `meta` keyed `app/name` → strings) is the high-watermark; events with `Timestamp <= cursor` are skipped. `mapMachineEvent` is a **strict allowlist**: only crashes (`exit` with non-zero `exit_code` and not `requested_stop`), OOMs (`exit` with `oom_killed=true`), and failing health checks return an `event.Event`. Routine `start/restart/launch/update/stop/destroy` events return `(zero, false)` and are silently dropped — they're either covered by `KindDeploy`, `KindCapacityDegraded`, or `KindCrashLoop`. Clean exits (`requested_stop` or `exit_code=0`) are also silent because they're indistinguishable from deploy/scale stops.
- **Bootstrap** (`Poller.bootstrap=true` on first pass only): cursors are advanced but no events are emitted. This is why a fresh start is silent — delete `notifier.db` to replay from "now".
- **Deploy detection** is separate from machine events: `detectDeploy` uses `uniformImage` (the image_ref shared by *every* machine, or `""` if any disagree) and compares against `meta["image_ref"]`. A change emits a single `KindDeploy` event, not one per machine. Returning `""` during mixed-image states deliberately stalls detection until the deploy converges — otherwise the stored cursor would flip back and forth as machines swap one by one.
- **Deploy-aware capacity suppression** (two layers, both required):
  - **Divergence-based**: `deployInProgress` (in `poller.go`) flags a poll as mid-deploy when machines have mixed `image_ref`s OR any machine diverges from the stored baseline. `capacityTracker.observe` consumes this flag — while true, it updates HWM but suppresses degraded/restored emits and resets both streak counters. Computed *before* `detectDeploy` updates the store, so the final poll of a deploy (when convergence just happened) still reads as "deploying" and doesn't leak a stray "capacity restored".
  - **Hysteresis**: `degradedRequired=2` consecutive `running<HWM` observations before firing the first degraded transition. Bridges a real Fly behavior where the API drops the running count to N-1 *before* the new machine's `image_ref` is visible — divergence-based detection is blind to that single poll. The second poll either sees divergence (deploying=true → streak reset → silent) or, for a true outage, confirms the drop and fires.
  - Both have a shared **safety timeout** (`defaultDeploySafetyTimeout=15m`): past it, the suppression lifts and normal alerting resumes so a wedged deploy stranded at half-capacity still surfaces — silent failure is worse than a noisy one. (Hysteresis still gates the first transition even after the timeout — two more polls needed.)
- **Crash loop tracking** (`crash_tracker.go`): per-machine sliding-window counter. After ≥3 crashes/OOMs in 10 min, emits a single `KindCrashLoop` and enters a 10-min cooldown. While in cooldown, `processMachineEvents` calls `crashes.inCooldown(...)` BEFORE `observe(...)` — if true, the **individual** crash event is suppressed (the loop alert is the consolidated signal). `observe` still records every crash so the count stays accurate across cooldown windows.
- **Capacity tracking** (`capacity_tracker.go`): per-app HWM of running machines, with two enhancements over a naive transition emit. (a) **Re-alert**: if `running < HWM` for ≥10 min the alert re-fires as "STILL degraded — N min elapsed", so a long-lived degradation stays visible instead of scrolling past. (b) **Flap suppression**: declaring "restored" requires `defaultHealthyStreakRequired` (=2) consecutive observations at HWM. A crash-looping machine bouncing between started/stopped each poll would otherwise produce a degraded ↔ restored ping-pong. HWM is in-memory; bootstrap pass re-seeds via `seed()` without emitting.
- **`internal/digest`** is a cron-driven snapshotter (UTC, `robfig/cron/v3`). It calls the same `flyapi.Client.ListMachines` independently of the poller, summarizes per-app state/region/check counts into a `Snapshot`, and emits a single `KindDigest` event carrying the snapshot in `Event.Payload`. Acts as the heartbeat — if digests stop arriving, the notifier or its connectivity is broken.
- **`internal/slack`** is the only consumer. `Dispatcher.handle` does in-memory dedup (sha256 over kind+app+machine+region+sorted fields, default 5-minute window), formats via `FormatEvent` → Block Kit JSON, and POSTs with retry on 429/5xx (honors `Retry-After`, exponential backoff capped at 30s, max 4 retries). **Digest events bypass dedup** so the recurring summary doesn't collapse to one message.
- **`internal/event`** defines the normalized `Event` struct that decouples producers from the dispatcher. `Fields map[string]string` is rendered into Slack section fields in a stable priority order (see `orderedFieldBlocks`); Slack caps at 10 fields per section so the priority list matters. Several `Kind` constants (`KindMachineStarted`, `KindMachineStopped`, `KindMachineExit`, `KindMachineCreated`, `KindMachineDestroyed`, `KindMachineEvent`, `KindHealthCheckPassing`) are defined but no longer produced by the poller — kept as exported symbols so dependents and historical events in stores stay typed.
- **`internal/config`** loads YAML, expands `${VAR}` from env (after `.env` is loaded — `LoadDotenv` does **not** override already-set env vars), applies defaults, validates. `Duration` is a custom type wrapping `time.Duration` with YAML unmarshaling.

### Things that look like bugs but aren't

- The poller's first call inside `Run` is `pollAll` *before* the ticker — this is the bootstrap pass. `bootstrap` flips to `false` immediately after.
- `slack.Dispatcher.dedup` is unbounded only in pathological cases; expired entries are purged opportunistically on each `isDuplicate` call.
- `slack.SlackConfig.Routing` is parsed but unused — reserved for future per-app channel routing.
- The event channel uses non-blocking `select` (`p.emit`); if it fills up (256 buffered), events are dropped with a warning rather than blocking the poll loop.
- "Machine started" / "machine restarted" do **not** produce Slack messages — this is intentional. If you want to see those, look at the digest or the Fly dashboard. The poller still logs every event at INFO via `slog` so the runtime logs of the notifier remain a complete audit trail.
- `KindCapacityRestored` may take ~one extra poll to arrive after the running count first hits HWM — that's `defaultHealthyStreakRequired=2` doing flap suppression.
- `mapMachineEvent` returns `(zero, false)` for the vast majority of incoming Fly events. Don't add an "unknown event" fallback — the previous behavior of emitting a generic `:information_source:` for every unmatched type/status was the single biggest source of channel noise.

## Conventions

- Module path: `github.com/benjbdev/flyio-slack-notifier`. Use this prefix for new internal imports.
- All packages use `slog` with a `component` attribute; new components should follow (`logger.With("component", "foo")`).
- Cron schedules are **UTC** (set explicitly in `main.go`), not local time.
- Tests are colocated (`*_test.go`) and hermetic — they fake the Fly API with `httptest` and use temp BoltDB files. Don't introduce real network calls in unit tests.
- **Commits and PR titles use [Conventional Commits](https://www.conventionalcommits.org/)** (`type(scope): description`, lowercase, imperative). Common types here: `feat`, `fix`, `refactor`, `perf`, `docs`, `test`, `ci`, `chore`. Scope is the package or area (`poller`, `slack`, `digest`, `flyapi`, `config`). Add a `BREAKING CHANGE:` footer when downstream Slack consumers would notice the shape change (e.g. removing a `Kind` from the emitted stream).
