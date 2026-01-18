# AGENTS.md

This repo is a two-process system:
- Go WhatsApp bridge: `whatsapp-bridge/` (WhatsApp Web connection + SQLite + HTTPS API)
- Python MCP server: `whatsapp-mcp-server/` (MCP tools; reads via Go HTTPS API; writes via Go HTTPS API)

Key docs to read first:
- `CLAUDE.md` (project architecture + constraints + dev commands)
- `README.md` (setup / running)

No Cursor rules found:
- No `.cursorrules`
- No `.cursor/rules/`

No Copilot rules found:
- No `.github/copilot-instructions.md`

---

## Build / Run / Lint / Test

### Go bridge (`whatsapp-bridge/`)

Run (recommended; generates self-signed TLS certs if missing):
- `./launch.sh`

Run (direct; assumes certs exist and env vars are configured):
- `go run main.go`

Build:
- `go build -o whatsapp-bridge main.go`

Format:
- `go fmt ./...`

Dependencies:
- `go mod tidy`

Tests:
- No `*_test.go` files in this repo currently.
- If/when tests are added:
  - Run all: `go test ./...`
  - Run a single test by name: `go test ./... -run '^TestName$'`
  - Run tests in one package: `go test ./path/to/pkg -run '^TestName$'`

Common runtime caveats:
- The bridge uses `github.com/mattn/go-sqlite3` which requires CGO; Windows needs `CGO_ENABLED=1` + a C toolchain.
- The HTTPS server refuses to start unless `WHATSAPP_BRIDGE_API_KEY` is set.
- Cert/key default to `whatsapp-bridge/store/server.crt` and `whatsapp-bridge/store/server.key`.

### Python MCP server (`whatsapp-mcp-server/`)

Install/sync deps:
- `uv sync`

Run:
- `uv run main.py`

Tests:
- No `test_*.py` files in this repo currently.
- If/when tests are added with pytest:
  - Run all: `uv run pytest`
  - Run a single test: `uv run pytest path/to/test_file.py -k 'test_name_substring'`
  - Run a single test node: `uv run pytest path/to/test_file.py::TestClass::test_name`

Lint/format:
- No linter/formatter is configured in-repo (no Ruff/Black config; no CI workflows).
- If you add tooling, prefer one toolchain (Ruff is a good default) and document it here.

---

## Local End-to-End Smoke Test

1) Start the bridge:
- `cd whatsapp-bridge && ./launch.sh`

2) Start the MCP server (in another terminal):
- `cd whatsapp-mcp-server && uv run main.py`

3) Validate from your MCP client (Claude Desktop / Cursor):
- Call read tools (e.g. list chats/messages) and ensure SQLite reads succeed.
- Call write tools (send message / send file) and ensure HTTPS API calls succeed.

---

## Configuration / Secrets

Secrets and generated artifacts are intentionally gitignored:
- `.env` is ignored by root `.gitignore`
- SQLite `*.db` is ignored
- TLS cert/key are ignored: `whatsapp-bridge/store/server.crt`, `whatsapp-bridge/store/server.key`

Env vars used by the Go bridge:
- `WHATSAPP_BRIDGE_API_KEY` (required; bridge refuses to start API server without it)
- `WHATSAPP_BRIDGE_TLS_CERT` (optional; defaults to `store/server.crt`)
- `WHATSAPP_BRIDGE_TLS_KEY` (optional; defaults to `store/server.key`)

Env vars used by the Python server:
- `WHATSAPP_BRIDGE_API_URL` (default `https://localhost:8080/api`)
- `WHATSAPP_BRIDGE_API_KEY` (should match bridge; code currently has a fallback default)
- `WHATSAPP_BRIDGE_TLS_VERIFY` (`true`/`false`; default `false` to tolerate self-signed cert)

Security note:
- Avoid committing API keys, `.env`, databases, or downloaded media.

---

## Code Style (Project-Conformant)

This repo is small and not formally tooled. Follow the existing patterns rather than introducing new conventions midstream.

### Python (`whatsapp-mcp-server/*.py`)

Formatting:
- 4-space indentation; keep line lengths reasonable (there is no enforced formatter).
- Prefer explicit, readable code over cleverness; keep functions focused.

Imports:
- Standard library first, then third-party, then local modules.
- Avoid unused imports; keep imports at module top.

Types:
- Python version is `>=3.11` per `whatsapp-mcp-server/pyproject.toml`.
- Prefer type hints on public functions and dataclasses.
- Use `Optional[T]` where `None` is a valid value; avoid ambiguous return types.

Naming:
- Functions/vars: `snake_case`
- Dataclasses/classes: `PascalCase`
- Constants: `UPPER_SNAKE_CASE`

Error handling:
- Prefer catching specific exceptions (`requests.RequestException`).
- When validating user inputs, raise `ValueError` with actionable messages (existing pattern).
- Avoid silent failures; return structured `{success, message}` to the MCP layer when appropriate.
- Don’t introduce empty `except:` blocks.

I/O and logging:
- Existing code uses `print(...)` for error/status output; if you add logging, keep it minimal and consistent.

HTTP/TLS:
- API calls use `requests.post(..., verify=WHATSAPP_API_VERIFY_TLS)`; local dev often runs with TLS verify off.
- Keep timeouts explicit (`WHATSAPP_API_TIMEOUT_SECONDS`).

### Go (`whatsapp-bridge/*.go`)

Formatting:
- Use `gofmt` (`go fmt ./...`). Don’t hand-format.

Naming:
- Exported identifiers: `PascalCase`
- Unexported identifiers: `camelCase`

Errors:
- Follow the existing pattern: return errors with context via `fmt.Errorf("...: %v", err)`.
- Check `err != nil` immediately; don’t ignore returned errors.

HTTP API:
- Endpoints are implemented with `net/http` handlers.
- Auth is enforced via `X-API-Key` header; keep auth checks early in handlers.

Files/paths:
- Bridge stores state under `whatsapp-bridge/store/`.
- Don’t change DB schema casually; it’s shared with the Python reader.

---

## Repo Conventions / Boundaries

- Keep changes minimal and scoped; avoid refactors while fixing bugs.
- Prefer reusing existing dependencies (Go modules + uv-managed Python deps).
- If you add new dev tooling (lint/test), wire it into docs and keep commands consistent across components.
