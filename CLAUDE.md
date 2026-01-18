# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

WhatsApp MCP Server - A Model Context Protocol (MCP) server that enables AI assistants to interact with WhatsApp. Connects to personal WhatsApp accounts via the WhatsApp Web multidevice API and stores message history locally in SQLite.

## Architecture

Two-component system:

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`):
   - Handles WhatsApp Web API connection using whatsmeow library
   - Manages authentication (QR code on first run)
   - Stores messages/chats in SQLite (`store/messages.db`)
   - Provides HTTP API on localhost:8080 for message sending and media downloads
   - Runs continuously to maintain WhatsApp connection

2. **Python MCP Server** (`whatsapp-mcp-server/`):
   - FastMCP-based server implementing MCP protocol
   - Queries SQLite database directly for reads
   - Calls Go bridge HTTP API for writes (send message, download media)
   - Communicates with Claude/Cursor via stdio transport

### Data Flow

- **Reading**: MCP Server → SQLite database → Return results
- **Sending**: MCP Server → Go bridge HTTP API → WhatsApp API
- **Receiving**: WhatsApp API → Go bridge → SQLite database → Available to MCP Server

### Database Schema

SQLite database (`whatsapp-bridge/store/messages.db`) contains:
- `chats` table: JID (primary key), name, last_message_time
- `messages` table: id+chat_jid (composite primary key), sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length

### JID Format

WhatsApp uses JID (Jabber ID) identifiers:
- Individual chats: `{phone_number}@s.whatsapp.net`
- Group chats: `{group_id}@g.us`
- Phone numbers have no `+` or symbols (e.g., "12025551234")

## Common Development Commands

### Running the System

```bash
# Start the Go WhatsApp bridge (must run continuously)
cd whatsapp-mcp/whatsapp-bridge
go run main.go

# First run: scan QR code with WhatsApp mobile app
# Re-authentication needed approximately every 20 days
```

The Python MCP server is launched automatically by Claude Desktop/Cursor via the configured `uv run` command.

### Go Bridge Development

```bash
cd whatsapp-mcp/whatsapp-bridge

# Run the bridge
go run main.go

# Build binary
go build -o whatsapp-bridge main.go

# Update dependencies
go mod tidy

# Windows: Enable CGO for sqlite3
go env -w CGO_ENABLED=1
```

### Python MCP Server Development

```bash
cd whatsapp-mcp/whatsapp-mcp-server

# Run server manually (for testing)
uv run main.py

# Update dependencies
uv add <package>

# Sync dependencies
uv sync
```

## Key Implementation Details

### Media Handling

- Media metadata stored in database by default (type, filename, message_id, chat_jid)
- Actual media files downloaded on-demand via `download_media` tool
- Audio messages: require FFmpeg for conversion to .ogg Opus format (WhatsApp voice message format)
- Without FFmpeg: audio files sent as raw documents via `send_file`

### Message Context

The `list_messages` function supports context retrieval:
- `include_context`: Include messages before/after matches
- `context_before`/`context_after`: Number of context messages
- Used for search results to provide conversation context

### Authentication

- Uses whatsmeow library's store mechanism (`whatsapp-bridge/store/whatsapp.db`)
- QR code displayed in terminal on first run
- Session persists until approximately 20 days of inactivity
- To reset: delete both `store/messages.db` and `store/whatsapp.db`

### HTTP API Endpoints

Go bridge exposes localhost:8080/api endpoints:
- `/api/send`: POST with recipient and message
- `/api/send-file`: POST with recipient and media_path
- `/api/send-audio`: POST with recipient and media_path
- `/api/download`: POST with message_id and chat_jid

## MCP Tools Available

Search/Retrieval:
- `search_contacts`: Find contacts by name/number
- `list_messages`: Query messages with filters, pagination, context
- `list_chats`: List chats with sorting/filtering
- `get_chat`: Get chat metadata by JID
- `get_direct_chat_by_contact`: Find 1-on-1 chat by phone number
- `get_contact_chats`: All chats involving a contact
- `get_last_interaction`: Most recent message with contact
- `get_message_context`: Messages before/after a specific message

Actions:
- `send_message`: Send text to individual or group
- `send_file`: Send image/video/document/raw audio
- `send_audio_message`: Send voice message (requires .ogg opus or FFmpeg)
- `download_media`: Download media from message, returns file path

## Important Constraints

- Go bridge must run continuously to maintain WhatsApp connection
- CGO required on Windows for sqlite3 (install MSYS2/MinGW)
- FFmpeg optional but required for audio message conversion
- Message history loads gradually after initial authentication
- Media downloaded on-demand (not stored locally by default)
- Respects WhatsApp's linked device limits
