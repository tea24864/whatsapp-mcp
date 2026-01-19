# WhatsApp Remote MCP (Go) Consolidation Plan

## Goals

- Replace the current two-process system (Go bridge + Python MCP adapter) with a single **remote** Go MCP server.
- Use the official MCP Go SDK: https://github.com/modelcontextprotocol/go-sdk.
- Authenticate all requests with `Authorization: Bearer <api-key>`.
- Support a single WhatsApp identity (one QR pairing) and persist session state.
- Provide **structured JSON** outputs (no legacy compatibility constraints).
- Support media download via **temporary HTTPS URLs** and enforce **10 minute** retention.

## Non-goals (v1)

- Backward compatibility with existing Python MCP tool names/semantics.
- Audio transcoding (voice messages must already be Opus/Ogg).
- Multiple WhatsApp identities.
- Multi-tenant isolation.

## Current State (Baseline)

- Go process (`whatsapp-bridge/`) maintains WhatsApp connection, SQLite stores, and exposes HTTPS `/api/*`.
- Python process (`whatsapp-mcp-server/`) exposes MCP over stdio and proxies calls to the Go HTTPS API.

Consolidation deletes the Python layer and hosts MCP directly in the Go service.

## Target Architecture

### Processes

- One long-running Go process:
  - WhatsApp connection + event handlers
  - SQLite state
  - MCP server over **Streamable HTTP** mounted at a configurable path (default: `/mcp`)
  - Media fetch + storage + garbage collection
  - Media download endpoint (default: `/media/*`)

### Network

- Public URL: `https://www.teamsaid.com/mcp` (Traefik + Let’s Encrypt)
- Traefik forwards to Go over plain HTTP.
- No header rewriting; `Authorization` passed through.

### Authentication

- Require `Authorization: Bearer <api-key>` for:
  - MCP endpoint (`/mcp`)
  - media endpoint (`/media/*`)
  - health endpoints (optional; can be unauthenticated if you prefer)

Implementation detail:
- Use middleware that validates:
  - header exists
  - token equals configured API key
  - (optional) constant-time compare to reduce timing leakage

## API / Tool Design (New, JSON-first)

Principles:
- Prefer `chat_jid` as the canonical identifier.
- If `chat_jid` is not provided, accept a phone number and attempt to resolve.
- If resolution is ambiguous or not found, return a tool error that forces explicit classification by the caller.
- For remote agents, avoid returning server-local file paths.

### Tools (v1 minimal set)

Note: Claude Desktop remote connectors currently require tool names to match `^[a-zA-Z0-9_-]{1,64}$` (no dots). This plan uses underscore-separated tool names.

1. `chats_list`
   - Input: `{ query?: string, limit?: number, cursor?: string }`
   - Output: `{ chats: ChatSummary[], next_cursor?: string }`

2. `chats_resolve_recipient`
   - Input: `{ phone_number: string }`
   - Output (success): `{ chat_jid: string, match_type: "exact" | "normalized" }`
   - Output (error): include candidates `{ candidates: ChatCandidate[] }`

3. `messages_list`
   - Input: `{ chat_jid: string, limit?: number, before?: string, after?: string, query?: string }`
   - Output: `{ messages: Message[] }`

4. `messages_send_text`
   - Input: `{ chat_jid?: string, phone_number?: string, text: string }`
   - Behavior:
     - If `chat_jid` present: send.
     - Else attempt resolve via `phone_number`.
     - If ambiguous/unknown: return error with candidates.
   - Output: `{ message_id?: string, status: "sent" | "failed", detail?: string }`

5. `messages_send_media_from_url`
   - Input: `{ chat_jid?: string, phone_number?: string, url: string, filename?: string, mime_type?: string, caption?: string }`
   - Behavior:
     - Fetch media server-side.
     - Store temporarily (<=10 min).
     - Send via WhatsApp.
   - Output: `{ message_id?: string, status: "sent" | "failed", detail?: string }`

6. `messages_send_voice_ogg_from_url`
   - Input: `{ chat_jid?: string, phone_number?: string, url: string }`
   - Constraints:
     - Only accept Opus/Ogg. If not, return error describing the required format.
   - Output: `{ message_id?: string, status: "sent" | "failed", detail?: string }`

7. `media_get_download_url`
   - Input: `{ chat_jid: string, message_id: string }`
   - Behavior:
     - Download media from WhatsApp if needed.
     - Store temporarily (<=10 min).
     - Return an HTTPS URL served by this service.
   - Output: `{ url: string, expires_at: string, media?: { filename?: string, mime_type?: string, size_bytes?: number } }`

Data model notes:
- `before/after` should be based on the DB’s message ordering fields (message timestamp or internal ID). Choose one canonical cursor format and keep it stable.

## Recipient Resolution Policy

- Source of truth: contact/recipient resolution (not fuzzy name search).
- Normalization:
  - Convert phone numbers to a canonical E.164-like form (best-effort).
  - Match against stored chat/contact identifiers in DB.
- Outcomes:
  - 0 matches: return error with guidance.
  - 1 match: return chat_jid.
  - >1 match: return error with candidates.

## Media URL + Retention Design

### Endpoints

- `GET /media/{media_id}`
  - Requires `Authorization: Bearer <api-key>`.
  - Returns bytes with correct `Content-Type` and `Content-Disposition`.

### Storage

- Store downloaded/fetched media under `whatsapp-bridge/store/tmp-media/` (or a new `store/tmp-media/` in consolidated layout).
- Track metadata:
  - `media_id` (opaque random ID)
  - file path
  - mime type
  - original filename
  - expires_at

### Garbage collection

- Run a periodic GC goroutine:
  - interval: e.g. 60s
  - delete expired files
  - delete associated metadata

Enforce retention:
- Max TTL: 10 minutes
- `media_get_download_url` returns `expires_at`.
- `GET /media/{media_id}` returns 404 after expiry.

## HTTP Server & Routing

- Use an HTTP mux/router to mount:
  - MCP handler at configurable path (`/mcp` default)
  - media handler at configurable path prefix (`/media` default)
  - optional `GET /healthz`, `GET /readyz`

MCP handler:
- Construct `mcp.Server`
- Register tools
- Wrap with auth middleware
- Serve via `mcp.NewStreamableHTTPHandler(getServer, opts)`

## Configuration

Add env vars (names are suggestions; pick consistent prefixes):

- `WHATSAPP_MCP_LISTEN_ADDR` (default `:8080` behind Traefik)
- `WHATSAPP_MCP_BASE_URL` (default `https://www.teamsaid.com`)
- `WHATSAPP_MCP_MCP_PATH` (default `/mcp`)
- `WHATSAPP_MCP_MEDIA_PATH_PREFIX` (default `/media`)
- `WHATSAPP_MCP_API_KEY` (required)
- `WHATSAPP_MCP_MEDIA_TTL_SECONDS` (default `600`)

Keep WhatsApp-related config already used by bridge as needed.

## Traefik Deployment Checklist

- Route `Host(`www.teamsaid.com`) && PathPrefix(`/mcp`)` to Go backend.
- Route `Host(`www.teamsaid.com`) && PathPrefix(`/media`)` to Go backend.
- Ensure streaming is not buffered for `/mcp`.
- Increase idle timeouts for long-lived MCP sessions.
- Pass through `Authorization` header.

## Implementation Steps

1. Repo restructure
   - Decide whether to:
     - keep code under `whatsapp-bridge/` and delete `whatsapp-mcp-server/`, or
     - move bridge code into a new root Go module.
   - Keep changes minimal: start by integrating MCP into `whatsapp-bridge/main.go`.

2. Add MCP Go SDK
   - Add dependency `github.com/modelcontextprotocol/go-sdk/mcp`.
   - Add minimal MCP server initialization.

3. Implement auth middleware
   - Validate Bearer token equals configured API key.
   - Wrap:
     - MCP HTTP handler
     - media HTTP handler

4. Implement media store
   - Temp file storage + metadata index.
   - TTL enforcement (10 min).
   - Background GC.

5. Implement tool handlers
   - Define input/output structs for each tool.
   - Ensure tool handlers return structured JSON.
   - Ensure phone-number resolution:
     - resolve to chat_jid if unique
     - else return error with candidates

6. Wire up HTTP server
   - Mount MCP handler at `/mcp` (configurable).
   - Mount media handler at `/media` (configurable).
   - Add health endpoints.

7. Manual verification (no tests currently exist)
   - Start server locally behind direct HTTP.
   - Validate MCP handshake and tool listing.
   - Call tools:
     - list chats
     - resolve recipient
     - send text
     - download media -> URL -> fetch bytes with Authorization
   - Verify TTL expiry behavior.

8. Cleanup
   - Remove Python server (`whatsapp-mcp-server/`) and associated docs only after Go MCP parity is acceptable.

## Open Decisions (Intentionally Deferred)

- Whether to keep any `/api/*` REST endpoints after MCP is complete.
- Whether to later add audio transcoding (FFmpeg or Go-native) for voice messages.
- Whether to support multipart upload/base64 for media in addition to URL fetch.
- Whether to implement rate limiting, audit logs, and per-tool authorization scopes.
