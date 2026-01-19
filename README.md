# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal Whatsapp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the Whatsapp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that project injection could lead to private data exfiltration.

## Installation

### Prerequisites

- Go
- Anthropic Claude Desktop app
- FFmpeg (_optional_) - Only needed if you want to convert audio to Opus/Ogg before sending voice messages.

### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/lharries/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Run the server**

   Navigate to the whatsapp-bridge directory and run the Go application:

   ```bash
   cd whatsapp-bridge
   ./launch.sh
   ```

   The first time you run it, you will be prompted to scan a QR code. Scan the QR code with your WhatsApp mobile app to authenticate.

   After approximately 20 days, you may need to re-authenticate.

3. **Connect Claude Desktop (Windows) via Cloudflare Tunnel**

   Claude Desktop "Connectors" require a publicly reachable HTTPS URL (localhost URLs are rejected), so for local development we tunnel the WSL server using Cloudflare.

   1) Install cloudflared (Windows PowerShell):

   ```powershell
   winget install -e --id Cloudflare.cloudflared
   ```

   2) Start the WhatsApp MCP server in WSL:

   ```bash
   cd whatsapp-bridge

   # Required in normal mode.
   export WHATSAPP_MCP_API_KEY="devkey"

   # Optional but recommended for correct media download URLs.
   # Set this later once you know the tunnel URL.
   # export WHATSAPP_MCP_BASE_URL="https://<your-tunnel-hostname>"

   ./launch.sh
   ```

   3) Start the tunnel from Windows (PowerShell). This will print a `https://...trycloudflare.com` URL:

   ```powershell
   cloudflared tunnel --url https://localhost:8080 --no-tls-verify
   ```

   4) Set `WHATSAPP_MCP_BASE_URL` in WSL to the printed tunnel URL (so `media_get_download_url` returns reachable links), then restart the server:

   ```bash
   export WHATSAPP_MCP_BASE_URL="https://<your-tunnel-hostname>"
   ```

   5) In Claude Desktop:

   - Settings -> Connectors -> Add custom connector
   - URL: `https://<your-tunnel-hostname>/mcp`
   - Headers:
     - `Authorization: Bearer devkey`

   Tool names use underscore-only (no dots), e.g. `chats_list`, `messages_send_text`, `media_get_download_url`.

### Windows + WSL Notes

- The Go server is expected to run in WSL (Ubuntu) and be tunneled to Claude Desktop running on Windows.
- `cloudflared` runs on Windows and forwards to the WSL server.
- You do not need to compile Go on Windows for this workflow.

## Architecture Overview

This application is a single Go process:

- **Go WhatsApp MCP Server** (`whatsapp-bridge/`): Connects to WhatsApp via whatsmeow, stores message history in SQLite, exposes MCP over Streamable HTTP at `/mcp`, and serves temporary media downloads at `/media/*`.

### Data Storage

- All message history is stored in a SQLite database within the `whatsapp-bridge/store/` directory
- The database maintains tables for chats and messages
- Messages are indexed for efficient searching and retrieval

## Usage

Once connected, you can interact with your WhatsApp contacts through Claude, leveraging Claude's AI capabilities in your WhatsApp conversations.

### Quick Smoke Tests

From Windows PowerShell, you can validate that the tunnel is up and the server is reachable:

```powershell
curl.exe -vk https://<your-tunnel-hostname>/healthz
curl.exe -vk -H "Authorization: Bearer devkey" -H "Accept: text/event-stream" https://<your-tunnel-hostname>/mcp
```

### MCP Tools

The server exposes a JSON-first toolset:

- `chats.list`
- `chats.resolve_recipient`
- `messages.list`
- `messages.send_text`
- `messages.send_media_from_url`
- `messages.send_voice_ogg_from_url`
- `media.get_download_url`

### Media Handling

- Sending media: `messages.send_media_from_url` fetches the URL server-side and sends it.
- Sending voice notes: `messages.send_voice_ogg_from_url` only accepts Opus/Ogg.
- Downloading media: `media.get_download_url` returns a temporary HTTPS URL under `/media/*` that expires (default 10 minutes).

## Technical Details

1. Your MCP client connects to the Go server over HTTPS at `/mcp` (Streamable HTTP).
2. The Go server queries the local SQLite database for reads and uses whatsmeow for WhatsApp operations.
3. Media downloads are exposed as temporary URLs under `/media/*` and require `Authorization: Bearer <api-key>`.

## Troubleshooting

- Make sure the Go server is running and connected to WhatsApp.
- Ensure your MCP client is sending `Authorization: Bearer <WHATSAPP_MCP_API_KEY>`.

### Authentication Issues

- **QR Code Not Displaying**: If the QR code doesn't appear, try restarting the authentication script. If issues persist, check if your terminal supports displaying QR codes.
- **WhatsApp Already Logged In**: If your session is already active, the Go bridge will automatically reconnect without showing a QR code.
- **Device Limit Reached**: WhatsApp limits the number of linked devices. If you reach this limit, you'll need to remove an existing device from WhatsApp on your phone (Settings > Linked Devices).
- **No Messages Loading**: After initial authentication, it can take several minutes for your message history to load, especially if you have many chats.
- **WhatsApp Out of Sync**: If your WhatsApp messages get out of sync with the bridge, delete both database files (`whatsapp-bridge/store/messages.db` and `whatsapp-bridge/store/whatsapp.db`) and restart the bridge to re-authenticate.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues). The documentation includes helpful tips for checking logs and resolving common issues.
