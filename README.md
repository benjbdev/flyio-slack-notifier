# flyio-slack-notifier

Self-hosted Slack notifier for Fly.io. Polls the Fly Machines API and
posts deploy / lifecycle / health-check events plus a recurring status
digest to a Slack channel via an Incoming Webhook.

App-level log errors are intentionally **not** monitored — assume those
go to Sentry or similar.

## What you get in Slack

- **Deploys** — image ref change across an app's machines
- **Machine lifecycle** — created, started, stopped, exited, OOM-killed,
  destroyed
- **Health-check transitions** — failing / passing
- **Status digest** — recurring summary (default every minute, switch to
  hourly for production) showing per-app machine count by state, region
  distribution, failing checks, latest image. Also acts as a heartbeat:
  if it stops arriving, the notifier or its connectivity is broken.

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

If you'd rather not poke at a real app, create a disposable one:

```bash
mkdir /tmp/notifier-test && cd /tmp/notifier-test
cat > Dockerfile <<'EOF'
FROM nginx:alpine
EOF
fly launch --name notifier-test --org <your-org> --no-deploy --copy-config=false
fly deploy
```

`fly launch` registers the app (so it shows up in the API) and writes
a `fly.toml`; without that step `fly deploy` errors with `app not
found`. After it's deployed, the same `fly machine restart` flow
above works.

Tear it down with `fly apps destroy notifier-test`.

## Tests

Hermetic unit tests (no network, no Fly account, no Slack workspace):

```bash
make test
```

Covers config parsing + `${VAR}` interpolation, BoltDB roundtrip,
poller bootstrap suppression / event emission / deploy detection,
digest summarization, Slack dispatcher formatting / dedup / retry on
429/5xx.

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

## Project layout

```
cmd/notifier/main.go        # entrypoint: load config, wire components, signals
internal/config/            # YAML + ${VAR} loader, .env reader
internal/event/             # normalized Event type + Kind/Severity enums
internal/flyapi/            # Machines REST client
internal/poller/            # 30s poll loop, state diff, deploy detection, BoltDB cursor
internal/digest/            # cron-driven status summarizer
internal/slack/             # Block Kit formatter, dedup, POST + retry
```

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
