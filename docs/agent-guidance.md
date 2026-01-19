# Agent Guidance (Canonical)

This is the single source of truth for agentic coding guidance in this repository.

If you are reading this via `AGENTS.md` or `CLAUDE.md`, those files are pointers.

---

## Project Overview

WhatsApp MCP Server: a Model Context Protocol (MCP) server for WhatsApp.

Architecture: single-process Go system implemented at repo root:
- Connects to WhatsApp via the WhatsApp Web multidevice API using whatsmeow
- Stores chats/messages in SQLite
- Exposes MCP over Streamable HTTP (default `/mcp`)
- Serves temporary media downloads under `/media/*`

Key docs:
- `README.md` (setup, Cloudflare tunnel notes)

AI rule files:
- No `.cursorrules`
- No `.cursor/rules/`
- No `.github/copilot-instructions.md`

---

## Build / Run / Lint / Test

All Go commands should be run from the repo root (this directory contains `go.mod`).

Run (recommended; generates self-signed TLS certs if missing):
- `./launch.sh`

Run (direct; assumes certs exist and env vars are configured):
- `go run main.go`

Build:
- `go build -o whatsapp-mcp main.go`

Format (required):
- `go fmt ./...`

Dependencies:
- `go mod tidy`

Tests:
- There are currently no `*_test.go` files in this repo.
- When tests exist:
  - Run all tests: `go test ./...`
  - Run a single test by name (regex): `go test ./... -run '^TestName$'`
  - Run a single package + test: `go test ./path/to/pkg -run '^TestName$'`
  - Common flags: `-v` (verbose), `-count=1` (disable cache)

Lint / static checks:
- No dedicated linter is configured in this repo.
- Optional local sanity check: `go vet ./...`

---

## Environment Variables

Required:
- none

Optional (common):
- `WHATSAPP_MCP_LISTEN_ADDR` (default `:8080`)
- `WHATSAPP_MCP_MCP_PATH` (default `/mcp`)
- `WHATSAPP_MCP_MEDIA_PATH_PREFIX` (default `/media`)
- `WHATSAPP_MCP_BASE_URL` (default `https://localhost:<port>`; set when tunneling so media URLs are reachable)
- `WHATSAPP_MCP_MEDIA_TTL_SECONDS` (default `600`)
- `WHATSAPP_MCP_TLS_CERT` (default `store/server.crt`)
- `WHATSAPP_MCP_TLS_KEY` (default `store/server.key`)

Debug / escape hatches (use with care):
- `WHATSAPP_MCP_DISABLE_TLS=true` (recommended behind Cloudflare Tunnel)

---

## HTTP Endpoints

- MCP: `GET/POST /mcp`
- Media: `GET /media/{media_id}`
- Health: `GET /healthz`
- Readiness: `GET /readyz`

---

## MCP Tools

Tool names use underscores (not dots):
- `chats_list`
- `chats_resolve_recipient`
- `messages_list` (supports optional context via `include_context`, `context_before`, `context_after`)
- `messages_send_text`
- `messages_send_media_from_url`
- `messages_send_voice_ogg_from_url` (Opus/Ogg only)
- `media_get_download_url` (returns temporary download URL)

---

## Local End-to-End Smoke Test

1) Start the server:
- `./launch.sh`

2) Validate HTTP endpoints (examples):
- `curl http://localhost:8080/healthz`
- `curl http://localhost:8080/readyz`

3) Validate from your MCP client:
- MCP endpoint is typically `/mcp`
- For media downloads: call `media_get_download_url` then `GET /media/{media_id}`

---

## Storage / Schema

- State lives under `store/`.
- WhatsApp session DB: `store/whatsapp.db`
- Messages DB: `store/messages.db` (schema is created in code).
- Avoid schema changes unless you also plan a migration.

Database tables (high level):
- `chats` (jid primary key)
- `messages` (id + chat_jid composite primary key)

JID formats:
- Individual: `{phone_number}@s.whatsapp.net`
- Group: `{group_id}@g.us`

---

## Code Style (Project-Conformant)

This repo is small and not formally tooled; follow existing patterns rather than introducing new conventions midstream.

Go formatting and imports:
- Always run `go fmt ./...` on Go changes.
- Let `gofmt` handle import grouping; keep unused imports out.

Naming:
- Exported identifiers: `PascalCase`
- Unexported identifiers: `camelCase`
- JSON DTO types commonly use a `DTO` suffix (e.g. `MessageDTO`).

Types / struct tags:
- Prefer explicit types over cleverness.
- Use `json:"field_name"` tags; use `omitempty` only when semantically correct.

Error handling:
- Check `err != nil` immediately; never ignore returned errors.
- Prefer contextual wrapping: `fmt.Errorf("<what failed>: %v", err)`.
- Close resources reliably (e.g. `defer rows.Close()`; close DB on init failure).

HTTP:
- HTTP endpoints are implemented with `net/http` handlers on a `ServeMux`.

Concurrency and safety:
- Protect shared mutable maps with `sync.RWMutex` (see tmp media index).
- Prefer small, scoped goroutines; ensure they can stop via context when appropriate.

---

## Repo Conventions / Boundaries

- Keep changes minimal and scoped; avoid refactors while fixing bugs.
- Prefer reusing existing dependencies.
- Generated runtime artifacts (DBs, TLS certs) must remain untracked.

---

## Configuration / Secrets

Secrets and generated artifacts are intentionally gitignored:
- `.env`
- SQLite `*.db`
- `store/` (includes session DBs and TLS cert/key)

Never commit API keys, `.env`, databases, or downloaded media.

---

## Platform Notes

- `github.com/mattn/go-sqlite3` requires CGO; on Windows you need `CGO_ENABLED=1` plus a C toolchain.
  - Optionally: `go env -w CGO_ENABLED=1` (still requires a C toolchain, e.g. MSYS2/MinGW)
- `./launch.sh` uses `openssl` to generate a self-signed cert/key if missing.
