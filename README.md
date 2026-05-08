# flyio-slack-notifier

[![CI](https://github.com/benjbdev/flyio-slack-notifier/actions/workflows/ci.yml/badge.svg)](https://github.com/benjbdev/flyio-slack-notifier/actions/workflows/ci.yml)

Self-hosted Slack notifier for Fly.io. Polls the Fly Machines API and
posts deploy / lifecycle / health-check events plus a recurring status
digest to a Slack channel via an Incoming Webhook.

App-level log errors are intentionally **not** monitored — those belong
in your error-tracking tool of choice, not here.

## What you get in Slack

The notifier sends a **signal-only** stream — every message is meant to
be actionable. Routine machine lifecycle chatter (start, restart,
launch/created, update/replacing, clean exits) is silently dropped
because that traffic dwarfs the alerts that actually matter when
something goes wrong. If you want to confirm "the machine came back",
check the next status digest or the Fly dashboard.

- **Deploys** — image ref change across an app's machines (a single
  rocket message per deploy, not per machine).
- **OOM kills** — `request.exit_event.oom_killed: true` in Fly's machine
  events. Critical-severity. The Fly Machines API does **not** emit a
  standalone "oom" event; the notifier parses the nested exit payload
  to distinguish OOM from a clean exit.
- **Crashes** — non-zero exit code without `requested_stop`. Surfaces
  SIGSEGV, V8 abort-on-OOM, and other unexpected process deaths that
  `oom_killed` alone doesn't cover. Once a crash-loop alert fires for
  a machine, individual crash events for that machine are suppressed
  for 10 minutes — the loop alert is the consolidated signal.
- **Crash loops** — the same machine sees ≥3 crash/OOM events inside a
  10-minute sliding window. Critical-severity. Deliberately separate
  from individual crashes: one OOM is a capacity hint, three in ten
  minutes is "stop trying to recover, the resource ceiling is too low".
- **Capacity degraded / restored** — per app, the notifier tracks the
  high-water-mark of running machines observed since startup and emits
  when running count drops below it (degraded, critical). If capacity
  stays below HWM, the alert **re-fires every 10 minutes** as
  "STILL degraded" so a long-lived shortfall can't get lost in chat
  scrollback. "Restored" only fires after **two consecutive healthy
  polls** to ride out crash-loop flap (degraded ↔ restored on every
  poll while a machine ping-pongs). HWM is in-memory and re-seeds on
  a notifier restart.
- **Health-check failing** — critical. Passing/recovery transitions
  are silent (covered by the inverse signal of the failing alert
  going quiet + the digest).
- **Status digest** — recurring summary (default hourly) showing
  per-app machine count by state, region distribution, failing
  checks, latest image. Also acts as a heartbeat: if it stops
  arriving, the notifier or its connectivity is broken. Digests
  always go through — never deduped.

What's intentionally **not** monitored / sent:

- **Routine machine lifecycle** (start, restart, launch, update,
  clean exit, destroy) — these accompany every deploy and every
  auto-recovery and turn the channel into a wall of green checkmarks.
  Deploys are one rocket message; capacity loss is a re-firing
  degraded alert; a process crashing is a crash event. Anything else
  is plumbing.
- **App-level error logs** — those belong in your error-tracking tool.
- **Memory pressure short of an OOM kill** (sustained high heap usage,
  Mark-Compact GC pauses). Needs the Fly metrics endpoint — a separate
  feature, not driven by the Machines API events stream.

## Prerequisites

- Go 1.26+
- A Fly.io account with at least one running app
- A Slack workspace where you can create an app (or admin who can
  approve one)

## Setup

### 1. Get your Fly API token

The notifier needs read-only access to list machines for the apps you
configure. The right token is an **org-scoped read-only token**:

```bash
fly tokens create readonly <org-slug>
```

Find your org slug with `fly orgs list`. The output is a long string
starting with `FlyV1 fm2_…` — that whole string is your `FLY_API_TOKEN`.

This token can list machines and read events for any app in that org,
but cannot deploy, destroy, or modify anything. Use one token per org
you want to monitor.

#### Quick alternative for getting started

```bash
fly auth token
```

This returns your **personal access token**. It works, but it carries
all your permissions across every org you belong to — fine for local
testing, over-broad for anything left running.

#### What *not* to use

A per-app deploy token (`fly tokens create deploy --app …`) is scoped
to a single app, but the notifier expects one token to read every app
in `apps:` — so deploy tokens are the wrong shape here.

### 2. Get a Slack incoming webhook URL

1. Go to <https://api.slack.com/apps> (signed in to your workspace).
2. **Create New App** → **From scratch** → name it (e.g. `Fly.io
   Notifier`), pick your workspace, **Create App**.
3. Left sidebar → **Incoming Webhooks** → toggle **Activate Incoming
   Webhooks** to **On**.
4. Scroll down → **Add New Webhook to Workspace** → choose the channel
   (e.g. `#fly-notif`) → **Allow**.
5. Copy the URL under **Webhook URLs for Your Workspace**. It looks
   like:
   ```
   https://hooks.slack.com/services/T01.../B02.../abc123XYZ...
   ```

This URL is your `SLACK_WEBHOOK_FLY_NOTIF`. It is permanently bound to
that channel.

If your workspace requires admin approval for new apps, the admin will
get an email — they click **Approve** and step 4 becomes available.

### 3. Create `.env`

```bash
cp .env.example .env
```

Edit `.env`:

```
FLY_API_TOKEN=<paste from step 1>
SLACK_WEBHOOK_FLY_NOTIF=<paste from step 2>
```

`.env` is gitignored.

### 4. Create `config.yaml`

```bash
cp config.example.yaml config.yaml
```

Edit the `apps:` list to match the apps you want monitored:

```yaml
apps:
  - name: api-prod
  - name: worker-staging
```

`fly apps list` shows what you have.

## Quick verification (before running the notifier)

```bash
source .env
curl -X POST -H 'Content-Type: application/json' \
  --data '{"text":"hello from curl"}' "$SLACK_WEBHOOK_FLY_NOTIF"
```

You should see `hello from curl` in your Slack channel and `curl`
prints `ok`. If you get `invalid_payload` or `channel_not_found`, the
URL is wrong.

## Run

```bash
make dev          # go run, no rebuild step
# or
make build && ./notifier --config config.yaml
```

The first poll **bootstraps**: it records the current machine state
without emitting anything, so you don't get spammed with the entire
historical event log on first start. After that, every 30s (default),
new events are forwarded to Slack.

Stop with Ctrl+C. State persists in `./notifier.db` (BoltDB) so a
restart doesn't replay events.

To fully reset state:

```bash
rm notifier.db
```

## Trigger events to test end-to-end

You don't need a deploy to verify the pipeline — a `fly machine
restart` produces stop + start lifecycle events, which is enough.

### Easiest: use an existing app

Pick any app you already run, put it in `config.yaml`, then in another
terminal:

```bash
fly apps list                            # pick one
fly machine list    -a <app>
fly machine restart <id> -a <app>        # → "machine stopped" + "machine started" in Slack
```

You should see both events arrive in `#fly-notif` within 30 seconds
(one poll cycle).

> **Token / org gotcha:** `fly apps list` uses your personal CLI
> credentials and may show apps in orgs your `FLY_API_TOKEN` cannot
> see. If the notifier logs `app "<name>" not found` for an app that
> shows up in `fly apps list`, the token's org doesn't cover that app.
> Either move the app, or generate a new readonly token for that org.

With `digest.schedule: "* * * * *"`, you'll also see a digest message
every minute regardless of whether anything changed.

### Optional: create a throwaway app

```bash
mkdir /tmp/notifier-test && cd /tmp/notifier-test
echo 'FROM nginx:alpine' > Dockerfile
fly launch --name notifier-test --org <your-org> --no-deploy --copy-config=false
fly deploy
```

`fly launch` registers the app (so the API can see it) before `fly
deploy`; same `fly machine restart` flow as above. Clean up with
`fly apps destroy notifier-test`.

## Using the published image

Every push to `main` and every `v*` tag publishes a multi-arch
(linux/amd64 + linux/arm64) image to GitHub Container Registry:
`ghcr.io/benjbdev/flyio-slack-notifier`.

### Tags

| Tag | When emitted | When to use |
|---|---|---|
| `sha-<short>` | every commit on `main` | **production** — fully deterministic |
| `vX.Y.Z` / `vX.Y` | on a semver tag push | production — pinned releases |
| `latest` | every push to `main` | local smoke tests only — never prod |

### How the image is wired

- Entrypoint: `notifier --config /app/config.yaml`
- Working directory: `/app` (image-baked, not a mount target — see below)
- BoltDB state file: configurable via `state_file:` in `config.yaml`.
  Set it to a path on a **separate** volume mount (e.g. `/data/notifier.db`).
  **Do not mount the persistent volume at `/app`** — it shadows the
  injected `config.yaml` and triggers a `chowning … ENOENT` reboot
  loop on Fly. The deploy section below uses `/data` for this reason.
- Required env: `FLY_API_TOKEN`, `SLACK_WEBHOOK_FLY_NOTIF` (referenced
  as `${VAR}` from inside the mounted `config.yaml`)
- Runs as root inside the container — Fly volumes are root-owned by
  default, so no mount-permission gymnastics needed.

### Local smoke test

Spin up the image against your real Fly account + Slack channel before
deploying anywhere:

```bash
cat > /tmp/notifier.yaml <<'YAML'
fly:
  api_token: ${FLY_API_TOKEN}
apps:
  - name: api-prod
slack:
  default_webhook: ${SLACK_WEBHOOK_FLY_NOTIF}
poll_interval: 30s
dedup_window: 5m
state_file: /data/notifier.db
digest:
  enabled: true
  schedule: "* * * * *"
YAML

docker run --rm \
  -e FLY_API_TOKEN="$FLY_API_TOKEN" \
  -e SLACK_WEBHOOK_FLY_NOTIF="$SLACK_WEBHOOK_FLY_NOTIF" \
  -v /tmp/notifier.yaml:/app/config.yaml:ro \
  -v notifier-data:/data \
  ghcr.io/benjbdev/flyio-slack-notifier:latest
```

A bind-mount of a single file at `/app/config.yaml` doesn't trigger
the volume-shadow problem — Docker overlays the file, not the
directory. The Fly issue is specific to mounting a whole volume on
top of `/app`.

### Deploy on Fly

Pin to a SHA tag in `fly.toml`:

```toml
app = "my-fly-notifier"
primary_region = "cdg"

[build]
  image = "ghcr.io/benjbdev/flyio-slack-notifier:sha-abc1234"

[[mounts]]
  source = "notifier_data"
  destination = "/data"

[[files]]
  guest_path = "/app/config.yaml"
  local_path = "config.yaml"

[[vm]]
  size = "shared-cpu-1x"
  memory = "256mb"
```

The volume mount destination **must not collide** with `/app` (the
image's WORKDIR) or with any path Fly's `[[files]]` block writes to.
Fly writes injected files first, then mounts the volume on top — if
the destinations overlap, the mount shadows the file and the boot
sequence reboot-loops on `chowning file ... ENOENT`. Mount the
volume at a separate path like `/data` and point `state_file:` in
your `config.yaml` at it (`state_file: /data/notifier.db`).

One-time setup:

```bash
fly apps create my-fly-notifier

fly volumes create notifier_data --size 1 --region cdg --app my-fly-notifier

fly secrets set --app my-fly-notifier \
  FLY_API_TOKEN="$(fly tokens create readonly <org-slug>)" \
  SLACK_WEBHOOK_FLY_NOTIF=https://hooks.slack.com/services/...

# First deploy: flyctl can't deploy an image-based fly.toml to an empty
# app. It errors with "could not create a fly.toml from any machines".
# Bootstrap with an explicit machine create instead:
fly machine run \
  --app my-fly-notifier \
  --region cdg \
  --vm-size shared-cpu-1x --vm-memory 256 \
  --volume notifier_data:/data \
  --file-local /app/config.yaml=config.yaml \
  ghcr.io/benjbdev/flyio-slack-notifier:sha-abc1234

# (One-time) reconcile under the `app` process group so future
# `fly deploy` calls see the machine. The first `fly deploy` creates
# a *second* machine because the bootstrap one has no process-group
# label; destroy the bootstrap machine + its orphan volume after.
fly deploy --app my-fly-notifier
fly machine list --app my-fly-notifier      # find the no-group machine
fly volumes list --app my-fly-notifier      # find its orphan volume
fly machine destroy <bootstrap-machine-id> --force --app my-fly-notifier
fly volumes destroy <orphan-volume-id> --app my-fly-notifier
```

After this dance, future SHA bumps are a clean `fly deploy`.

Notes:

- `${FLY_API_TOKEN}` and `${SLACK_WEBHOOK_FLY_NOTIF}` references inside
  the mounted `config.yaml` resolve from the Fly secrets at process
  start — no rebuild needed when secrets rotate.
- Single machine is fine; the notifier doesn't need HA. Two would
  duplicate every Slack message.
- The image is publicly readable on GHCR — no registry auth needed in
  `fly.toml`.

### Updating

Bump the `image = "...:sha-<new>"` line in `fly.toml` and run
`fly deploy`. The volume preserves `notifier.db`, so the cursor carries
across — no replay of historical events.

## Tests

```bash
make test
```

Hermetic — no network, no Fly account, no Slack workspace.

## Configuration reference

```yaml
fly:
  api_token: ${FLY_API_TOKEN}     # required
  base_url: https://api.machines.dev   # optional override

apps:                              # required, at least one
  - name: api-prod
  - name: worker-staging

slack:
  default_webhook: ${SLACK_WEBHOOK_FLY_NOTIF}   # required
  routing: {}                      # optional; reserved for future use

poll_interval: 30s                 # how often to poll the Machines API
dedup_window: 5m                   # suppress identical events within this window
state_file: ./notifier.db          # where the event cursor is persisted

digest:
  enabled: true
  schedule: "* * * * *"            # cron, UTC. Use "0 * * * *" for hourly.
```

`${VAR}` references in the YAML are expanded from the process
environment at startup (after `.env` is loaded).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `config load failed: fly.api_token is required` | `.env` not present in working dir, or `FLY_API_TOKEN` empty in `.env` |
| `slack post: status 404: invalid_token` or `no_service` | webhook URL wrong or the Slack app was uninstalled |
| Notifier logs `app "<name>" not found` | (1) typo in `apps:` list, or (2) the app exists but in an org your `FLY_API_TOKEN` doesn't cover. Verify the app shows up under `fly orgs list` for the same org you ran `fly tokens create readonly` against |
| `fly deploy` itself errors with `app not found` | the app hasn't been registered yet — run `fly apps create <name>` or `fly launch --name <name> --no-deploy` first |
| No events on `fly deploy` | first poll is bootstrap (suppressed); also check `poll_interval` — give it 30s+ |
| Duplicate Slack messages | shouldn't happen within `dedup_window`; lengthen the window if you see them across restarts |
| Digest sent at unexpected times | cron schedule is in **UTC**, not local time |

## Notes

- The notifier uses the **public** `api.machines.dev` endpoint, so it
  can run anywhere with outbound HTTPS — laptop, VPS, Fly itself, etc.
- It does **not** subscribe to Fly's internal NATS log stream, so no
  WireGuard tunnel needed.
- One instance can monitor multiple Fly orgs if your token is scoped
  to all of them.
