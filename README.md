# job_tracker

> Türkçe dokümantasyon için: [README.tr.md](README.tr.md)

A local/VPS-friendly automation written in Go that periodically reads Gmail,
classifies job-application emails with an LLM, stores them in SQLite, shows
them in a web table, and pushes a phone notification via [ntfy.sh](https://ntfy.sh)
when a status changes.

- **Language:** Go 1.22+ (mostly standard library, minimal dependencies)
- **DB:** SQLite — pure-Go driver (`modernc.org/sqlite`), no CGO
- **Mail:** Official Gmail API + OAuth2 (not IMAP); near-real-time pickup via
  cheap Gmail history polling
- **Classification:** Pluggable `Classifier` interface — Google Gemini (free tier),
  OpenRouter (free models) or Anthropic Claude; extracts the actual employer,
  the position and the intermediary platform (LinkedIn, ATS, agency)
- **Deduplication:** one row per company+position — confirmations from a job
  board and the company itself merge, and a rejection folds into the same row
- **Pre-screening:** Keyword filter, optionally backed by a small free LLM (hybrid mode)
- **Notifications:** Telegram bot or ntfy.sh, behind a `Notifier` interface
- **UI:** `net/http` + `html/template`, server-rendered; rows expand to the
  application's full mail history with direct Gmail links

> ⚠️ **Security:** This repo is public. No secret is ever committed.
> `.env`, `credentials.json`, `token.json` and `*.db` are gitignored.

## Package layout

```
cmd/server        web UI (+ optional Basic Auth)
cmd/poller        fetch Gmail → classify → DB → notify (--once or ticker)
internal/config   config from env (.env supported)
internal/gmail    Gmail API client, OAuth, pre-filter
internal/classifier  LLM classifiers (interface + Gemini/OpenRouter/Anthropic + prompts)
internal/store    SQLite schema + repository (applications table)
internal/notifier ntfy notifications (interface + impl)
```

## Setup

### 0. Requirements
- Go 1.22+
- A Google account (Gmail) and at least one LLM API key
  (Gemini free tier is enough: https://aistudio.google.com/apikey)

### 1. Install dependencies

```bash
go mod download
```

### 2. Gmail API credentials (credentials.json)

1. [Google Cloud Console](https://console.cloud.google.com/) → create a new project.
2. **APIs & Services → Library** → "Gmail API" → **Enable**.
3. **APIs & Services → OAuth consent screen** → choose External, name the app,
   and add your own Gmail address under **Test users** (no need to publish).
4. **APIs & Services → Credentials → Create Credentials → OAuth client ID** →
   Application type: **Desktop app** → create.
5. Save the downloaded JSON to the project root as **`credentials.json`**.
   (It is gitignored; never commit it.)

### 3. Configuration (.env)

```bash
cp .env.example .env
```

Fill in the values — at minimum `GEMINI_API_KEY` (or another provider's key
plus `LLM_PROVIDER`). Every variable is documented in `.env.example`.

### 4. Generate the OAuth token (token.json)

Run once:

```bash
go run ./cmd/poller --auth
```

The command prints a URL. Open it in a browser and authorize your Gmail
account; the code is captured automatically by a local callback server and
**`token.json`** is saved (contains the refresh token; gitignored).

### 5. Notifications (optional)

**Telegram (recommended):**
1. Message [@BotFather](https://t.me/BotFather) on Telegram → `/newbot` → copy the **bot token**.
2. Send your new bot any message (it cannot message you first).
3. Open `https://api.telegram.org/bot<TOKEN>/getUpdates` in a browser and copy
   the `"chat":{"id":...}` value.
4. Set `TELEGRAM_BOT_TOKEN` and `TELEGRAM_CHAT_ID` in `.env`.

**ntfy.sh (alternative):** install the ntfy app, pick a hard-to-guess topic and
set `NTFY_TOPIC=https://ntfy.sh/<topic>`. Selection is automatic (`NOTIFIER`
overrides it); leaving everything empty disables notifications.

## Running locally

```bash
# Full-scan-process once (for testing/cron/backfill):
go run ./cmd/poller --once

# Real-time mode: initial full scan, then new mails are picked up within
# POLL_INTERVAL (default 45s) via cheap Gmail history polling:
go run ./cmd/poller

# Check LLM API health / quota status:
go run ./cmd/poller --check

# Web UI:
go run ./cmd/server
# → http://localhost:8080
```

In real-time mode only genuinely new mails are processed — the history call
returns just new message IDs and costs almost nothing, so rate limits are not
a concern. If the tracker was offline long enough for Gmail to forget the
stored history ID (~a week), it self-heals with a full scan.

## LLM providers and quotas

The classifier is selected with `LLM_PROVIDER` (`gemini` | `openrouter` | `anthropic`).
All providers are implemented behind the same `Classifier` interface.

- **Gemini** (default): `gemini-3.1-flash-lite` offers 15 requests/min and
  500 requests/day on the free tier. Per-minute (RPM) limit hits are retried
  automatically with the API-suggested delay.
- **OpenRouter**: models with the `:free` suffix cost nothing. Free-tier
  accounts get ~50 requests/day (1000/day once you hold $10+ in credits).
- **Anthropic**: paid; Haiku is the economical choice.

When the daily quota is exhausted the poller stops the cycle with a clear
error and unprocessed mails are picked up in the next cycle. To detect an
exhausted quota early:

```bash
go run ./cmd/poller --check                      # single cheap live request
LIVE_API_TEST=1 go test ./internal/classifier/ -v # live tests for all providers
```

## Classification prompt

The classification instructions live in `internal/classifier/prompt.go`.
Editing only that file is enough to change the LLM behavior.

The model must reply with this JSON schema:

```json
{"is_job_related": true, "company": "Acme", "status": "interview", "confidence": 0.92}
```

Records with `is_job_related=false` or `confidence < CONFIDENCE_THRESHOLD`
are not written to the DB.

## Pre-filter

A cheap screening runs before the LLM (`internal/gmail/filter.go`):

- Senders from `FILTER_EXCLUDE_DOMAINS` are dropped outright.
- A mail passes when one of `FILTER_KEYWORDS` appears in the subject/body
  (an empty list disables the filter).
- **Hybrid mode** (`PREFILTER_MODE=hybrid`): mails with no keyword match get a
  second look from a small free LLM via OpenRouter (`PREFILTER_MODEL`), which
  answers YES/NO. This catches application mails whose wording evades the
  keyword list, at the cost of a few free-tier requests.

## Tests

```bash
go test ./...
```

Unit tests use mocked HTTP servers and consume no API quota. Live API tests
are opt-in via `LIVE_API_TEST=1`.

## VPS deployment

1. **Build** (targeting Linux, from Windows/Mac):

   ```bash
   GOOS=linux GOARCH=amd64 go build -o bin/poller ./cmd/poller
   GOOS=linux GOARCH=amd64 go build -o bin/server ./cmd/server
   ```

   `modernc.org/sqlite` is pure Go, so there is no CGO/cross-compile pain.

2. **Move secrets safely** (never via the repo — use `scp`):

   ```bash
   scp .env credentials.json token.json user@vps:/opt/job_tracker/
   scp bin/poller bin/server user@vps:/opt/job_tracker/
   ```

   `token.json` contains the refresh token, so the poller runs headless on
   the VPS; no browser needed again.

3. **Schedule the poller** — two options:

   **a) systemd timer + `--once`** (recommended, cron-like):

   `/etc/systemd/system/job-poller.service`:
   ```ini
   [Unit]
   Description=Job Tracker Poller
   [Service]
   WorkingDirectory=/opt/job_tracker
   ExecStart=/opt/job_tracker/poller --once
   EnvironmentFile=/opt/job_tracker/.env
   ```
   `/etc/systemd/system/job-poller.timer`:
   ```ini
   [Unit]
   Description=Run job poller every 15m
   [Timer]
   OnBootSec=2m
   OnUnitActiveSec=15m
   [Install]
   WantedBy=timers.target
   ```
   ```bash
   sudo systemctl enable --now job-poller.timer
   ```

   **b) Continuous service (its own ticker):** `ExecStart=/opt/job_tracker/poller`
   (no flag) with `Restart=always` as a regular `.service`.

4. **Web UI service** `/etc/systemd/system/job-server.service`:
   ```ini
   [Unit]
   Description=Job Tracker Web
   [Service]
   WorkingDirectory=/opt/job_tracker
   ExecStart=/opt/job_tracker/server
   EnvironmentFile=/opt/job_tracker/.env
   Restart=always
   [Install]
   WantedBy=multi-user.target
   ```

5. **Web security:** if the UI is internet-facing, set `WEB_USER`/`WEB_PASS`
   in `.env` (enables HTTP Basic Auth) and put a reverse proxy (Caddy/Nginx)
   in front for TLS.

## License

For personal use. Mind your secret hygiene: this repo is public.
