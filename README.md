# Aestrl

> Portable, read-only, peer-to-peer file inspection tool.

[![Go](https://img.shields.io/badge/go-1.26+-00ADD8.svg)](https://golang.org)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![P2P](https://img.shields.io/badge/WebRTC-Pion%20Direct%20P2P-orange.svg)](https://github.com/pion/webrtc)
[![Search](https://img.shields.io/badge/Hybrid%20Search-BM25%20%2B%20Potion%20Embeddings-purple.svg)](https://github.com/trengrj/go-potion)

**Aestrl** allows users to inspect local and remote codebases through encrypted peer-to-peer data channels with zero cloud file storage.

Users run a single self-contained binary on their machine to temporarily share any project directory. Remote collaborators, terminal users, browsers, and AI agents inspect files, query code outlines, locate symbol definitions, and execute hybrid semantic searches directly on the host machine.

---

## Key Principles & Architecture

- **One Portable Binary**: Written in pure Go with embedded web UI assets (`go:embed`). No external runtime, database, or heavy dependencies.
- **Encrypted WebRTC P2P Transport**: CLI, TUI, and Web browsers communicate with the host directly over encrypted WebRTC DataChannels (`aestrl-inspect`). No file inspection payloads pass through cloud relays.
- **Unified 15 Core Operations**: Every interface (CLI, TUI, WebUI, and Hosted-AI) calls the exact same core engine dispatcher:
  `ls`, `tree`, `stat`, `find`, `search`, `grep`, `context`, `read`, `outline`, `raw`, `symbol`, `refs`, `deps`, `compare`, `manifest`.
- **Hybrid Retrieval (BM25 + CPU Semantic Embeddings)**: Fast lexical BM25 ranking coupled with CPU static embeddings using `go-potion` (`minishlab/potion-base-8M`), symbol weighting, and path relevance.
- **Local Snapshot Indexing & Persistent Vectors**: Local index cache (`~/.aestrl/index/<shareID>.json`) persists file hashes and embedding vectors, enabling delta re-indexing upon re-sharing.
- **Background Daemon & Multi-Share Management**: Local IPC socket (`~/.aestrl/daemon.sock`) manages multiple concurrent shares.
- **Cloudflare Coordination**: One Worker at `aestrl.dvrsfy.org` provides ephemeral discovery records, WebRTC SDP signaling relay, browser UI delivery, and Hosted-AI HTTP 307 redirects.
- **Removable Hosted-AI Adapter**: Optional HTTP/HTTPS interface with Let's Encrypt TLS and explicit HTTP fallback. Can be stripped at build time via `go build -tags noai`.

---

## Installation & Build

### Standard Build (Full)
```bash
go build -ldflags="-s -w" -o aestrl ./cmd/aestrl
```

### AI-Free Build (`noai`)
Excludes Hosted-AI HTTP server, certificate dependencies, and endpoints:
```bash
go build -tags noai -ldflags="-s -w" -o aestrl-noai ./cmd/aestrl
```

---

## Usage

### 1. Configuration (`setup`)
Configure persistent identity, coordinator URL, or directory sidecars:
```bash
aestrl setup                 # Global configuration (~/.config/aestrl/config.json)
aestrl setup --sidecar       # Local directory sidecar (.aestrl/config.json)
aestrl setup --export        # Export configuration as compact base64 string
aestrl setup --import <str>  # Import configuration string
```

### 2. Sharing Directories
```bash
# Share current working directory (runs via background daemon)
aestrl share

# Share a specific path with 4-hour lifetime and Hosted-AI adapter enabled
aestrl share ./my-project --ttl 4h --ai

# Share in foreground (Ctrl+C to terminate)
aestrl share ./my-project --foreground
```

### 3. Share Management
```bash
aestrl status                # List active shares, paths, remaining TTL, and peers
aestrl stop <session_id>     # Revoke a specific share
aestrl stop all              # Revoke all active shares
```

### 4. Interactive Terminal Explorer (TUI)
```bash
aestrl                       # Interactive TUI for current directory (when in terminal)
aestrl tui ./project         # Local directory TUI
aestrl tui <share_url>       # Remote encrypted P2P TUI
```

### 5. CLI Inspection (Local & Remote P2P)
All commands work identically on local directories or remote share URLs:
```bash
aestrl ls [url] [path]          # List directory items
aestrl tree [url] [path]        # Directory tree structure
aestrl stat [url] <path>        # File or directory metadata
aestrl find [url] <query>       # Find files by pattern
aestrl search [url] <query>     # Hybrid BM25 + semantic ranked search
aestrl grep [url] <pattern>     # Line-by-line regex or pattern search
aestrl context [url] <query>    # Packaged high-relevance evidence within token budget
aestrl cat [url] <path>         # Read file contents
aestrl outline [url] <path>     # Extract functions, structs, classes, and headings
aestrl raw [url] <path>         # Stream raw bytes (supports binary retrieval)
aestrl symbol [url] <name>      # Locate symbol definitions across files
aestrl refs [url] <name>        # Find symbol references and callers
aestrl deps [url] <path>        # Analyze import dependencies
aestrl compare [url] <p1> <p2>  # Compare diff between two files
aestrl manifest [url]           # Complete repository manifest
```

---

## Security Boundaries

1. **Jail Protection**: Real path evaluation and lexical directory containment reject symlink escapes and `../` traversal.
2. **Capability Validation**: Every inspection request over WebRTC DataChannel requires the cryptographic secret fragment (`#<secret>`).
3. **Strictly Read-Only**: Inspection engine contains zero write, delete, or shell execution capabilities.
4. **Zero Cloud File Transfer**: Inspection payloads and file contents NEVER touch Cloudflare or intermediary servers.
