# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal WhatsApp messages (including images, videos, documents, and audio messages), search your contacts, and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the WhatsApp Web multi-device API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that project injection could lead to private data exfiltration.

> *Warning:* this project is a work in progress. The intended setup is to run the MCP server remotely with OAuth2 in front of it; local/dev setup details may change.

## Installation

### Prerequisites

- Go
- Anthropic Claude Desktop app
- OpenSSL (used by `./launch.sh` to generate a self-signed TLS cert)
- FFmpeg (_optional_) - Only needed if you want to convert audio to Opus/Ogg before sending voice messages.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/tea24864/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Run the server**

   Run the Go application:

   ```bash
   ./launch.sh
   ```

   By default, the server:
   - listens on `:8080`
   - serves MCP at `/mcp`
   - serves temporary media downloads at `/media/<id>`
   - uses TLS with a self-signed cert (generated under `store/`)

   The first time you run it, you will be prompted to scan a QR code. Scan the QR code with your WhatsApp mobile app to authenticate.

   WhatsApp may require re-authentication periodically.

3. **Connect Claude Desktop (Windows) via Cloudflare Tunnel**

   If your MCP client requires a publicly reachable HTTPS URL (for example, Claude Desktop custom connectors), tunnel your local server using Cloudflare.

   1) Install cloudflared (Windows PowerShell):

   ```powershell
   winget install -e --id Cloudflare.cloudflared
   ```

   2) Start the WhatsApp MCP server in WSL (recommended: disable local TLS behind the tunnel):

   ```bash
   # Serve plain HTTP locally; Cloudflare provides HTTPS publicly.
   export WHATSAPP_MCP_DISABLE_TLS="true"

   # Optional but recommended for correct media download URLs.
   # Set this later once you know the tunnel URL.
   # export WHATSAPP_MCP_BASE_URL="https://<your-tunnel-hostname>"

   ./launch.sh
   ```

   3) Start the tunnel from Windows (PowerShell). This will print a `https://...trycloudflare.com` URL:

   ```powershell
   cloudflared tunnel --url http://localhost:8080
   ```

   4) Set `WHATSAPP_MCP_BASE_URL` in WSL to the printed tunnel URL (so `media_get_download_url` returns reachable links), then restart the server:

   ```bash
   export WHATSAPP_MCP_BASE_URL="https://<your-tunnel-hostname>"
   ```

   5) In Claude Desktop:

   - Settings -> Connectors -> Add custom connector
   - URL: `https://<your-tunnel-hostname>/mcp`
   
   Tool names use underscore-only (no dots), e.g. `chats_list`, `messages_send_text`, `media_get_download_url`.

### Docker + Cloudflare Tunnel (Recommended)

This repo ships a `compose.yaml` that runs:
- the Go MCP server (plain HTTP inside the Docker network)
- a `cloudflared` sidecar that provides public HTTPS

1) Create an env file (do not commit it):

```bash
cp compose.env.example .env
```

2) Fill in:
- `WHATSAPP_MCP_BASE_URL` (your public https tunnel hostname)
- `TUNNEL_TOKEN_WHATSAPP` (for a named tunnel)

3) Start:

```bash
docker compose -f compose.yaml up -d
```

For quick-tunnel development (random `trycloudflare.com` hostname):

```bash
docker compose --profile quick up -d whatsapp-mcp cloudflared-quick
```

Note: quick tunnels are best-effort/dev only; use a named tunnel for stability.

### Configuration

All configuration is via environment variables:

- `WHATSAPP_MCP_LISTEN_ADDR` (default `:8080`)
- `WHATSAPP_MCP_MCP_PATH` (default `/mcp`)
- `WHATSAPP_MCP_MEDIA_PATH_PREFIX` (default `/media`)
- `WHATSAPP_MCP_MEDIA_TTL_SECONDS` (default `600`)
- `WHATSAPP_MCP_BASE_URL` (default `https://localhost:<port>`; set this when tunneling so media URLs are reachable)
- `WHATSAPP_MCP_DISABLE_TLS=true` (serve plain HTTP; recommended behind Cloudflare Tunnel)
- `WHATSAPP_MCP_TLS_CERT` (default `store/server.crt`)
- `WHATSAPP_MCP_TLS_KEY` (default `store/server.key`)

## Architecture Overview

This application is a single Go process:

- **Go WhatsApp MCP Server**: Connects to WhatsApp via whatsmeow, stores message history in SQLite, exposes MCP over Streamable HTTP at `/mcp`, and serves temporary media downloads at `/media/*`.

### HTTP Endpoints

- MCP: `GET/POST /mcp` (Streamable HTTP)
- Media: `GET /media/<id>` (temporary downloads)
- Health: `GET /healthz`
- Readiness: `GET /readyz`

There are also some legacy/debug REST endpoints under `/api/*` (JSON POST) used for direct interaction outside MCP.

### Data Storage

- All message history is stored in a SQLite database within the `store/` directory
- The database maintains tables for chats and messages
- Messages are indexed for efficient searching and retrieval

## Usage

Once connected, you can interact with your WhatsApp contacts through Claude, leveraging Claude's AI capabilities in your WhatsApp conversations.

### Quick Smoke Tests

From Windows PowerShell, you can validate that the tunnel is up and the server is reachable:

```powershell
curl.exe -vk https://<your-tunnel-hostname>/healthz
curl.exe -vk -H "Accept: text/event-stream" https://<your-tunnel-hostname>/mcp
```

### MCP Tools

Tool names use underscores (not dots):

- `chats_list`
- `chats_resolve_recipient`
- `messages_list`
- `messages_send_text`
- `messages_send_media_from_url`
- `messages_send_voice_ogg_from_url`
- `media_get_download_url`

### Media Handling

- Sending media: `messages_send_media_from_url` fetches the URL server-side and sends it.
- Sending voice notes: `messages_send_voice_ogg_from_url` requires an `.ogg` URL (Opus-in-Ogg).
- Downloading media: `media_get_download_url` returns a temporary URL under `/media/<id>` that expires (default 10 minutes).

## Technical Details

1. Your MCP client connects to the Go server over HTTP(S) at `/mcp` (Streamable HTTP).
2. The Go server queries the local SQLite database for reads and uses whatsmeow for WhatsApp operations.
3. Media downloads are exposed as temporary URLs under `/media/<id>`.

## Troubleshooting

- Make sure the Go server is running and connected to WhatsApp.
- If you are tunneling via Cloudflare and used `cloudflared tunnel --url http://localhost:8080`, set `WHATSAPP_MCP_DISABLE_TLS=true` before starting the server.

### Authentication Issues

- **QR Code Not Displaying**: If the QR code doesn't appear, try restarting the authentication script. If issues persist, check if your terminal supports displaying QR codes.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: After initial authentication, it can take several minutes for your message history to load, especially if you have many chats.
- **WhatsApp Out of Sync**: If your WhatsApp messages get out of sync with the server, delete both database files (`store/messages.db` and `store/whatsapp.db`) and restart to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
