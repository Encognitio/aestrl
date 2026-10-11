# Aestrl

> Portable P2P File Inspector with Persistent MCP Identity

[![Go](https://img.shields.io/badge/go-1.26+-00ADD8.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![MCP](https://img.shields.io/badge/MCP-Streamable%20HTTP%20%7C%20SSE-brightgreen.svg)](https://modelcontextprotocol.io)
[![P2P](https://img.shields.io/badge/WebRTC-Pion%20Direct%20P2P-orange.svg)](https://github.com/pion/webrtc)

**Aestrl** allows users to temporarily share local directories with browsers, humans, ChatGPT, Codex, Gemini and other MCP-compatible agents with zero cloud uploads.

Users install one portable binary, run `aestrl setup` once to configure a persistent MCP identity in ChatGPT, and can thereafter share any project temporarily. When a share link is pasted into ChatGPT, the assistant immediately inspects files directly on the host machine.

---

## Key Principles & Architecture

- **Single Portable Binary**: Written in Go with embedded web UI assets (`go:embed`). No external runtime or dependencies.
- **Persistent MCP Routing**: ChatGPT connects to a stable endpoint (`https://aestrl.dvrsfy.org/<identity>/mcp`) once. No reinstalling integrations for new shares.
- **Outbound-Only Host Connection**: Hosts behind NAT/firewalls connect outbound over persistent TLS/WebSocket to the public hub. No open ports required.
- **Direct Browser P2P (WebRTC)**: Web browsers negotiate direct encrypted P2P data channels with the host via Pion WebRTC. Direct P2P is mandatory; no TURN relays.
- **Strictly Read-Only & Isolated**: Traversal and symlink escape protection, default secret/credential exclusions, bounded query sizes, and session revocation.
- **Zero Cloud File Persistence**: Files remain on the host machine. Hub only transiently proxies MCP queries; Cloudflare is never in the inspection data path.

### Traffic Flow

```text
[ ChatGPT / MCP Client ]
         |
         | HTTPS Streamable MCP
         v
[ Go Hub: aestrl.dvrsfy.org/<identity>/mcp ]
         |
         | Authenticated Outbound Persistent WebSocket
         v
[ Aestrl Host (User Machine) ]
         |
         v
[ Local Shared Directory ]
```

```text
[ Desktop / Mobile Browser ]
         |
         | WebRTC Signaling
         v
[ Direct Encrypted WebRTC Data Channel ]
         |
         v
[ Aestrl Host (User Machine) ]
```

---

## Installation

### Prebuilt Binaries
Download the latest release for your platform from GitHub Releases, or build from source:

```bash
git clone https://github.com/Encognitio/aestrl.git
cd aestrl
go build -ldflags="-s -w" -o aestrl ./cmd/aestrl
```

---

## 1. Quick Start

### Step 1: One-Time Persistent Setup

```bash
aestrl setup
```

Output:
```text
Aestrl configured.

Identity: h7af8ven8
Hub: https://aestrl.dvrsfy.org

Your permanent MCP endpoint:
https://aestrl.dvrsfy.org/h7af8ven8/mcp

ChatGPT / Custom MCP Client Setup:
1. In ChatGPT or your MCP client, configure custom MCP:
   - Server URL: https://aestrl.dvrsfy.org/h7af8ven8/mcp
   - Authentication: Bearer Token
   - Token: 3a9f02...

To share a directory, run:
  aestrl share ./project
```

### Step 2: Configure ChatGPT Once

1. Open ChatGPT -> **Settings** -> **Connected Apps** (or Custom Actions / MCP).
2. Add a new MCP server:
   - **URL**: `https://aestrl.dvrsfy.org/<identity>/mcp`
   - **Authentication**: Bearer Token / API Key
   - **Token**: `<mcp_token>` from setup
3. Save. This integration stays configured forever across restarts and different projects.

### Step 3: Share a Directory

```bash
aestrl share ./my-project --ttl 2h
```

Output:
```text
Aestrl sharing: /home/user/my-project

Share:
https://aestrl.dvrsfy.org/s/8f10b74ce12a45d0#c0a98f71b29e01f4...

MCP:
https://aestrl.dvrsfy.org/h7af8ven8/mcp

Expires: 2 hours
Press Ctrl+C to stop
```

### Step 4: Inspect in ChatGPT or Browser

- **In ChatGPT**: Paste the share URL (`https://aestrl.dvrsfy.org/s/...#...`) into your chat. ChatGPT calls `connect_share` and can immediately browse, search, and inspect the project files.
- **In Browser**: Click or open the share URL in Safari, Chrome, Firefox, or Edge (mobile & desktop) for real-time encrypted P2P inspection.

---

## CLI Inspection Commands

Inspect remote shares directly from the terminal without opening a browser:

```bash
# List directory contents
aestrl ls https://aestrl.dvrsfy.org/s/<session>#<secret> [path]

# Recursive tree with depth limit
aestrl tree https://aestrl.dvrsfy.org/s/<session>#<secret> [path] --depth 3

# Ripgrep-style content search (literal or regex) with line numbers
aestrl rg https://aestrl.dvrsfy.org/s/<session>#<secret> "currentToken" src/

# Read file or line range
aestrl cat https://aestrl.dvrsfy.org/s/<session>#<secret> src/main.go --lines 20:60

# Download remote file
aestrl get https://aestrl.dvrsfy.org/s/<session>#<secret> assets/logo.png -o ./logo.png
```

*Tip: Set `export AESTRL_SHARE="https://..."` to omit the URL from CLI commands.*

---

## Model Context Protocol (MCP) Tools

Aestrl exposes standard read-only MCP tools to ChatGPT and MCP clients:

| Tool | Description |
|------|-------------|
| `connect_share` | Authorizes a temporary share URL for the conversation. |
| `list_shares` | Lists all active shares under the persistent identity. |
| `list_directory` | Lists directory contents (files, sizes, types). |
| `tree` | Visual recursive directory tree with depth limits. |
| `search_files` | Substring and glob filename matching. |
| `search_content` | Regex and literal content search with line numbers and context. |
| `read_file` | Bounded file reading with optional 1-indexed line ranges. |
| `file_info` | Metadata, MIME type, and binary detection flags. |
| `get_file` | Retrieve file content within transfer bounds. |

---

## Self-Hosting the Hub

You can run your own Aestrl public hub on any VPS:

```bash
aestrl hub --listen :8080 --hub-url https://aestrl.yourdomain.com --data-dir /var/lib/aestrl
```

### Caddy Reverse Proxy Example

```caddy
aestrl.yourdomain.com {
    reverse_proxy localhost:8080
}
```

> **Important**: Configure `aestrl.yourdomain.com` as **DNS-only (Grey Cloud)** in Cloudflare to prevent Cloudflare from intercepting or proxying inspection data and WebSocket traffic.

---

## Security Guarantees

1. **Strictly Read-Only**: Aestrl has zero write, modify, or execute capabilities.
2. **Directory Root Boundary**: Uses `filepath.EvalSymlinks` and canonical prefix checking to block path traversal (`../`) and symlinks escaping the shared root.
3. **Automatic Secret Exclusions**: Automatically excludes `.git`, `.env*`, `*credential*`, `*secret*`, `*.pem`, `*.key`, `id_rsa*`, `node_modules`, `vendor`, build caches, and sensitive files.
4. **Transient Transit**: The hub never writes shared files to disk or caches file payloads.
5. **Capability Isolation**: Share secrets are cryptographically random 128-bit hex strings stored in the URL fragment (`#<secret>`), which is never sent over HTTP to the server.

---

## License

MIT License. See [LICENSE](LICENSE) for details.
