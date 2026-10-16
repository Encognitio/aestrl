package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Encognitio/aestrl/pkg/config"
	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/Encognitio/aestrl/pkg/host"
	"github.com/Encognitio/aestrl/pkg/p2p"
	"github.com/Encognitio/aestrl/pkg/tui"
	"golang.org/x/sys/unix"
)

const Version = "2.0.0"

func main() {
	if len(os.Args) < 2 {
		// If running in an interactive terminal, launch TUI on current directory
		if isTerminal(os.Stdin.Fd()) {
			runTUI([]string{"."})
			return
		}
		printUsage()
		return
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "setup":
		runSetup(args)
	case "share":
		runShare(args)
	case "daemon":
		runDaemon(args)
	case "status":
		runStatus(args)
	case "stop":
		runStop(args)
	case "tui":
		runTUI(args)
	case "ls", "list":
		runOp("ls", args)
	case "tree":
		runOp("tree", args)
	case "stat":
		runOp("stat", args)
	case "find":
		runOp("find", args)
	case "search":
		runOp("search", args)
	case "grep", "rg":
		runOp("grep", args)
	case "context":
		runOp("context", args)
	case "cat", "read":
		runOp("read", args)
	case "outline":
		runOp("outline", args)
	case "raw", "get":
		runOp("raw", args)
	case "symbol":
		runOp("symbol", args)
	case "refs":
		runOp("refs", args)
	case "deps":
		runOp("deps", args)
	case "compare":
		runOp("compare", args)
	case "manifest":
		runOp("manifest", args)
	case "version", "--version", "-v":
		fmt.Printf("aestrl version %s\n", Version)
	case "help", "--help", "-h":
		printUsage()
	default:
		// If argument looks like a path or share URL, launch TUI
		if isShareURL(cmd) || isDirectory(cmd) {
			runTUI(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "Unknown command '%s'. Run 'aestrl help' for usage.\n", cmd)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`Aestrl — Portable Read-Only Peer-to-Peer File Inspector (v%s)

Usage:
  aestrl                          Launch interactive TUI (when in terminal)
  aestrl setup [options]          Configure persistent identity, sidecar, or global settings
  aestrl share [dir]              Share a local directory (defaults to current dir)
  aestrl status                   Show active sharing sessions
  aestrl stop [session_id|all]    Revoke and stop active share session(s)
  aestrl tui [url|path]           Interactive terminal explorer for share or directory

Inspection Commands (support both local paths and remote share URLs):
  aestrl ls [url] [path]          List directory items
  aestrl tree [url] [path]        Show directory tree
  aestrl stat [url] <path>        Inspect file or directory metadata
  aestrl find [url] <query>       Find files matching pattern
  aestrl search [url] <query>     Hybrid BM25 and symbol ranked search
  aestrl grep [url] <pattern>     Regex or text pattern search
  aestrl context [url] <query>    Package high-relevance evidence for query
  aestrl cat [url] <path>         Read file content
  aestrl outline [url] <path>     Extract code outline (classes, functions, headings)
  aestrl raw [url] <path>         Stream or retrieve raw file bytes
  aestrl symbol [url] <name>      Locate symbol definitions across files
  aestrl refs [url] <name>        Find symbol references
  aestrl deps [url] <path>        Analyze import dependencies
  aestrl compare [url] <p1> <p2>  Compare two files
  aestrl manifest [url]           Generate complete repository manifest

Options:
  --ttl <duration>                Set share lifetime (default: 2h)
  --ai                            Enable Hosted-AI HTTP adapter
  --port <addr>                   Port/bind address for Hosted-AI adapter
  --domain <host>                 Domain for Let's Encrypt HTTPS on Hosted-AI adapter
  --allow-http                    Allow unencrypted HTTP fallback if HTTPS unavailable
  --foreground                    Run share in foreground instead of background daemon
  --json                          Format output as JSON
`, Version)
}

func runSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	hubURL := fs.String("hub", config.DefaultHubURL, "Cloudflare coordinator URL")
	sidecar := fs.Bool("sidecar", false, "Save configuration to local .aestrl/config.json in current directory")
	allowHTTP := fs.Bool("allow-http", true, "Allow unencrypted HTTP fallback for Hosted-AI adapter")
	ttlStr := fs.String("ttl", "2h", "Default share lifetime")
	reset := fs.Bool("reset", false, "Regenerate identity and credentials")
	exportStr := fs.Bool("export", false, "Export configuration as base64 string")
	importStr := fs.String("import", "", "Import configuration from base64 string")
	jsonOutput := fs.Bool("json", false, "Output in JSON format")
	_ = fs.Parse(args)

	targetPath := ""
	if *sidecar {
		if exeSidecar := config.ExeSidecarConfigPath(); exeSidecar != "" {
			targetPath = exeSidecar
		} else {
			targetPath = config.SidecarConfigPath(".")
		}
	} else {
		var err error
		targetPath, err = config.DefaultConfigPath()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Config path error: %v\n", err)
			os.Exit(1)
		}
	}

	if *exportStr {
		cfg, _, err := config.DiscoverConfig(".")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading configuration: %v\n", err)
			os.Exit(1)
		}
		encoded, err := cfg.Export()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Export error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(encoded)
		return
	}

	if *importStr != "" {
		cfg, err := config.Import(*importStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Import error: %v\n", err)
			os.Exit(1)
		}
		if err := cfg.Save(targetPath); err != nil {
			fmt.Fprintf(os.Stderr, "Failed saving imported config: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Configuration imported successfully to %s\n", targetPath)
		return
	}

	var cfg *config.Config
	if !*reset {
		if existing, _, err := config.DiscoverConfig("."); err == nil {
			cfg = existing
		}
	}

	if cfg == nil {
		var err error
		cfg, err = config.DefaultConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed generating default config: %v\n", err)
			os.Exit(1)
		}
	}

	if *hubURL != "" {
		cfg.HubURL = strings.TrimRight(*hubURL, "/")
	}
	if *ttlStr != "" {
		cfg.DefaultTTL = *ttlStr
	}
	cfg.AllowHTTP = *allowHTTP

	if err := cfg.Save(targetPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed saving config to %s: %v\n", targetPath, err)
		os.Exit(1)
	}

	if *jsonOutput {
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(data))
		return
	}

	fmt.Printf(`Aestrl configured.

Identity:    %s
Coordinator: %s
Config File: %s
Default TTL: %s
Allow HTTP:  %t

Run 'aestrl share' to share the current directory.
`, cfg.Identity, cfg.HubURL, targetPath, cfg.DefaultTTL, cfg.AllowHTTP)
}

func runShare(args []string) {
	fs := flag.NewFlagSet("share", flag.ExitOnError)
	ttlStr := fs.String("ttl", "2h", "Share time-to-live duration")
	enableAI := fs.Bool("ai", false, "Enable Hosted-AI HTTP adapter")
	aiAddr := fs.String("port", "", "Bind address for Hosted-AI adapter (e.g. :8443 or :8080)")
	aiDomain := fs.String("domain", "", "Public domain for Let's Encrypt HTTPS on Hosted-AI adapter")
	allowHTTP := fs.Bool("allow-http", false, "Explicitly approve unencrypted HTTP fallback if HTTPS fails")
	foreground := fs.Bool("foreground", false, "Run in foreground")
	hubURL := fs.String("hub", config.DefaultHubURL, "Cloudflare coordination URL")
	_ = fs.Parse(args)

	targetDir := "."
	if len(fs.Args()) > 0 {
		targetDir = fs.Args()[0]
	}

	absDir, err := filepath.Abs(targetDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving path: %v\n", err)
		os.Exit(1)
	}

	// Discover configuration from sidecar or OS fallback
	cfg, _, _ := config.DiscoverConfig(absDir)
	if *hubURL == config.DefaultHubURL && cfg != nil && cfg.HubURL != "" {
		*hubURL = cfg.HubURL
	}
	if *ttlStr == "2h" && cfg != nil && cfg.DefaultTTL != "" {
		*ttlStr = cfg.DefaultTTL
	}

	ttl, err := time.ParseDuration(*ttlStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid TTL duration '%s': %v\n", *ttlStr, err)
		os.Exit(1)
	}

	opts := host.ShareOptions{
		TTL:        ttl,
		EnableAI:   *enableAI,
		AIAddr:     *aiAddr,
		AIDomain:   *aiDomain,
		AllowHTTP:  *allowHTTP || (cfg != nil && cfg.AllowHTTP),
		HubURL:     *hubURL,
	}

	if *foreground {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		share, err := host.StartShare(ctx, absDir, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error starting share: %v\n", err)
			os.Exit(1)
		}
		defer share.Close()

		printShareDetails(&share.Info, true)

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		fmt.Println("\nStopping share...")
		return
	}

	// Connect via background daemon
	client, err := host.EnsureDaemon("", "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error contacting daemon: %v\n", err)
		os.Exit(1)
	}

	info, err := client.Share(absDir, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Share failed: %v\n", err)
		os.Exit(1)
	}

	printShareDetails(info, false)
}

func printShareDetails(info *host.ShareInfo, isForeground bool) {
	fmt.Printf("\nAestrl sharing: %s\n\n", info.RootDir)
	fmt.Printf("Share URL:\n%s\n\n", info.ShareURL)

	if info.AIURL != "" {
		fmt.Printf("Hosted-AI URL:\n%s/?secret=%s\n\n", info.AIURL, info.Secret)
	}

	fmt.Printf("Session ID: %s\n", info.SessionID)
	fmt.Printf("Expires:    %s (%s remaining)\n\n", info.ExpiresAt.Format(time.RFC1123), time.Until(info.ExpiresAt).Round(time.Second))

	if isForeground {
		fmt.Println("Press Ctrl+C to stop sharing.")
	} else {
		fmt.Println("Active in background daemon.")
		fmt.Printf("Run 'aestrl status' to view active shares, or 'aestrl stop %s' to revoke.\n", info.SessionID)
	}
}

func runDaemon(args []string) {
	d, err := host.NewDaemon("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start daemon: %v\n", err)
		os.Exit(1)
	}
	defer d.Close()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
}

func runStatus(args []string) {
	client := host.NewClient("")
	shares, err := client.Status()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to reach daemon: %v (no active shares)\n", err)
		return
	}

	if len(shares) == 0 {
		fmt.Println("No active shares. Run 'aestrl share [dir]' to share.")
		return
	}

	fmt.Printf("%-18s %-6s %-25s %s\n", "SESSION ID", "PEERS", "EXPIRES", "DIRECTORY")
	fmt.Println(strings.Repeat("-", 75))
	for _, s := range shares {
		remaining := time.Until(s.ExpiresAt).Round(time.Minute)
		fmt.Printf("%-18s %-6d %-25s %s\n", s.SessionID, s.PeerCount, remaining.String()+" left", s.RootDir)
		fmt.Printf("  └─ %s\n", s.ShareURL)
	}
}

func runStop(args []string) {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}

	client := host.NewClient("")
	if err := client.Stop(target); err != nil {
		fmt.Fprintf(os.Stderr, "Stop failed: %v\n", err)
		os.Exit(1)
	}

	if target == "" || target == "all" {
		fmt.Println("All active shares stopped.")
	} else {
		fmt.Printf("Share %s stopped.\n", target)
	}
}

func runTUI(args []string) {
	target := "."
	if len(args) > 0 {
		target = args[0]
	}

	if isShareURL(target) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		client, err := p2p.DialShare(ctx, target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to connect to remote share: %v\n", err)
			os.Exit(1)
		}
		defer client.Close()

		invoker := &tui.P2PInvoker{Session: client}
		_ = tui.Run(invoker, target, os.Stdin, os.Stdout)
		return
	}

	// Local directory TUI
	absPath, err := filepath.Abs(target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid directory: %v\n", err)
		os.Exit(1)
	}

	eng, err := engine.New(absPath, nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize engine: %v\n", err)
		os.Exit(1)
	}

	invoker := &tui.EngineInvoker{Eng: eng}
	_ = tui.Run(invoker, absPath, os.Stdin, os.Stdout)
}

func runOp(op string, args []string) {
	jsonOutput := false
	var filteredArgs []string
	for _, a := range args {
		if a == "--json" {
			jsonOutput = true
		} else {
			filteredArgs = append(filteredArgs, a)
		}
	}

	var shareTarget string
	var opArgs []string

	if len(filteredArgs) > 0 && isShareURL(filteredArgs[0]) {
		shareTarget = filteredArgs[0]
		opArgs = filteredArgs[1:]
	} else if envShare := os.Getenv("AESTRL_SHARE"); envShare != "" {
		shareTarget = envShare
		opArgs = filteredArgs
	} else {
		opArgs = filteredArgs
	}

	params := buildOpParams(op, opArgs)

	if shareTarget != "" {
		// Remote encrypted WebRTC P2P execution
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()

		client, err := p2p.DialShare(ctx, shareTarget)
		if err != nil {
			fmt.Fprintf(os.Stderr, "P2P connection error: %v\n", err)
			os.Exit(1)
		}
		defer client.Close()

		res, err := client.Execute(ctx, op, params)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Operation %s failed: %v\n", op, err)
			os.Exit(1)
		}

		printOpResult(op, res, jsonOutput)
		return
	}

	// Local execution
	eng, err := engine.New(".", nil, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Engine error: %v\n", err)
		os.Exit(1)
	}

	res, err := eng.Execute(op, params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Operation %s failed: %v\n", op, err)
		os.Exit(1)
	}

	printOpResult(op, res, jsonOutput)
}

func buildOpParams(op string, args []string) map[string]interface{} {
	params := make(map[string]interface{})
	switch op {
	case "ls":
		path := ""
		if len(args) > 0 {
			path = args[0]
		}
		params["path"] = path

	case "tree":
		path := ""
		depth := 3
		if len(args) > 0 {
			path = args[0]
		}
		if len(args) > 1 {
			if d, err := strconv.Atoi(args[1]); err == nil {
				depth = d
			}
		}
		params["path"] = path
		params["depth"] = depth

	case "stat", "outline", "deps":
		path := ""
		if len(args) > 0 {
			path = args[0]
		}
		params["path"] = path

	case "read":
		path := ""
		start := 0
		end := 0
		if len(args) > 0 {
			path = args[0]
		}
		if len(args) > 1 {
			start, _ = strconv.Atoi(args[1])
		}
		if len(args) > 2 {
			end, _ = strconv.Atoi(args[2])
		}
		params["path"] = path
		params["start_line"] = start
		params["end_line"] = end

	case "find", "search", "grep":
		query := ""
		path := ""
		if len(args) > 0 {
			query = args[0]
		}
		if len(args) > 1 {
			path = args[1]
		}
		params["query"] = query
		params["path"] = path

	case "context":
		query := ""
		budget := 4000
		if len(args) > 0 {
			query = args[0]
		}
		if len(args) > 1 {
			if b, err := strconv.Atoi(args[1]); err == nil {
				budget = b
			}
		}
		params["query"] = query
		params["budget"] = budget

	case "symbol", "refs":
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		params["name"] = name

	case "compare":
		p1, p2 := "", ""
		if len(args) > 0 {
			p1 = args[0]
		}
		if len(args) > 1 {
			p2 = args[1]
		}
		params["path1"] = p1
		params["path2"] = p2

	case "raw":
		path := ""
		if len(args) > 0 {
			path = args[0]
		}
		params["path"] = path
	}
	return params
}

func printOpResult(op string, res interface{}, jsonOutput bool) {
	if jsonOutput {
		data, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(data))
		return
	}

	data, _ := json.Marshal(res)

	switch op {
	case "ls":
		var items []engine.FileItem
		_ = json.Unmarshal(data, &items)
		for _, it := range items {
			if it.IsDir {
				fmt.Printf("%s/\n", it.Path)
			} else {
				fmt.Printf("%-40s %8d bytes\n", it.Path, it.Size)
			}
		}

	case "read":
		var rf engine.ReadFileResult
		if err := json.Unmarshal(data, &rf); err == nil && rf.Content != "" {
			fmt.Println(rf.Content)
			return
		}
		fmt.Println(string(data))

	case "raw":
		var raw engine.RawFileResult
		if err := json.Unmarshal(data, &raw); err == nil && len(raw.Data) > 0 {
			_, _ = os.Stdout.Write(raw.Data)
			return
		}
		fmt.Println(string(data))

	case "grep":
		var matches []engine.SearchMatch
		if err := json.Unmarshal(data, &matches); err == nil {
			for _, m := range matches {
				fmt.Printf("%s:%d: %s\n", m.Path, m.LineNumber, m.LineText)
			}
			return
		}
		fmt.Println(string(data))

	case "search":
		var results []engine.SearchResult
		if err := json.Unmarshal(data, &results); err == nil {
			for _, r := range results {
				fmt.Printf("%s:%d [%.2f] %s\n", r.Path, r.LineNumber, r.Score, r.Excerpt)
			}
			return
		}
		fmt.Println(string(data))

	default:
		indented, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(indented))
	}
}

func isShareURL(s string) bool {
	return strings.HasPrefix(s, "http://") ||
		strings.HasPrefix(s, "https://") ||
		(strings.Contains(s, "/s/") && strings.Contains(s, "#"))
}

func isDirectory(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	return err == nil
}
