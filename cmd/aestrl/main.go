package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Encognitio/aestrl/pkg/client"
	"github.com/Encognitio/aestrl/pkg/config"
	"github.com/Encognitio/aestrl/pkg/host"
	"github.com/Encognitio/aestrl/pkg/hub"
	"github.com/Encognitio/aestrl/web"
)

const Version = "1.0.1"

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "setup":
		runSetup(args)
	case "hub":
		runHub(args)
	case "share":
		runShare(args)
	case "ls":
		runLs(args)
	case "tree":
		runTree(args)
	case "rg":
		runRg(args)
	case "cat":
		runCat(args)
	case "get":
		runGet(args)
	case "version", "--version", "-v":
		fmt.Printf("aestrl version %s\n", Version)
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown command '%s'. Run 'aestrl help' for usage.\n", cmd)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`Aestrl — Portable P2P File Inspector with Persistent MCP Identity (v%s)

Usage:
  aestrl setup                     Establish persistent identity and MCP routing
  aestrl hub                       Run public MCP gateway and connection coordinator
  aestrl share <directory>         Temporarily share a local directory
  aestrl ls <share-url> [path]     List directory contents of a remote share
  aestrl tree <share-url> [path]   Display recursive tree structure
  aestrl rg <share-url> <pattern>  Search file contents (regex or literal)
  aestrl cat <share-url> <path>    Read remote file with optional line range
  aestrl get <share-url> <path>    Download remote file
  aestrl version                   Print version information

Options:
  Run 'aestrl <command> --help' for details on each subcommand.
`, Version)
}

func runSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	hubURL := fs.String("hub", config.DefaultHubURL, "Coordinator hub URL")
	reset := fs.Bool("reset", false, "Force generate a new identity instead of reusing existing")
	exportCfg := fs.Bool("export", false, "Export existing configuration string for another machine")
	importCfg := fs.String("import", "", "Import configuration string exported from another machine")
	jsonOutput := fs.Bool("json", false, "Output results in JSON format")
	_ = fs.Parse(args)

	cfgPath, err := config.DefaultConfigPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Handle import
	if *importCfg != "" {
		imported, err := config.Import(*importCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error importing config: %v\n", err)
			os.Exit(1)
		}
		if err := imported.Save(cfgPath); err != nil {
			fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Configuration successfully imported and saved.")
		printConfigInstructions(imported, *jsonOutput)
		return
	}

	// Handle export
	if *exportCfg {
		existing, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error loading existing config: %v\n", err)
			os.Exit(1)
		}
		encoded, err := existing.Export()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error exporting config: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(encoded)
		return
	}

	// Check if already configured
	if !*reset {
		if existing, err := config.Load(cfgPath); err == nil && existing.Identity != "" {
			targetHub := strings.TrimRight(existing.HubURL, "/")
			if *hubURL != config.DefaultHubURL {
				targetHub = strings.TrimRight(*hubURL, "/")
				existing.HubURL = targetHub
			}
			if err := verifyHubRegistration(targetHub, existing.Identity, existing.HostToken, existing.MCPToken); err != nil {
				fmt.Fprintf(os.Stderr, "Error: Existing identity verification failed against hub at %s: %v\nRun 'aestrl setup --reset' to generate a new identity.\n", targetHub, err)
				os.Exit(1)
			}
			printConfigInstructions(existing, *jsonOutput)
			return
		}
	}

	// Generate fresh identity and credentials
	identityID := config.GenerateIdentityID()
	hostToken, err := config.GenerateSecureToken(24)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating host token: %v\n", err)
		os.Exit(1)
	}
	mcpToken, err := config.GenerateSecureToken(24)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error generating MCP token: %v\n", err)
		os.Exit(1)
	}

	targetHub := strings.TrimRight(*hubURL, "/")
	if err := verifyHubRegistration(targetHub, identityID, hostToken, mcpToken); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to register persistent identity with hub at %s: %v\n", targetHub, err)
		os.Exit(1)
	}

	cfg := &config.Config{
		Identity:   identityID,
		HubURL:     targetHub,
		HostToken:  hostToken,
		MCPToken:   mcpToken,
		DefaultTTL: "2h",
		CreatedAt:  time.Now(),
	}

	if err := cfg.Save(cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
		os.Exit(1)
	}

	printConfigInstructions(cfg, *jsonOutput)
}

func verifyHubRegistration(hubURL, identity, hostToken, mcpToken string) error {
	regPayload := hub.SetupRequest{
		Identity:  identity,
		HostToken: hostToken,
		MCPToken:  mcpToken,
	}
	regBytes, err := json.Marshal(regPayload)
	if err != nil {
		return err
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	setupURL := fmt.Sprintf("%s/api/setup", hubURL)
	resp, err := httpClient.Post(setupURL, "application/json", bytes.NewReader(regBytes))
	if err != nil {
		return fmt.Errorf("network connection error: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var setupResp hub.SetupResponse
	if err := json.Unmarshal(body, &setupResp); err != nil {
		return fmt.Errorf("invalid hub JSON response: %w", err)
	}

	if setupResp.Status != "created" && setupResp.Status != "verified" {
		return fmt.Errorf("unexpected hub status '%s'", setupResp.Status)
	}
	return nil
}

func printConfigInstructions(cfg *config.Config, jsonOutput bool) {
	if jsonOutput {
		data, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(data))
		return
	}

	baseURL := strings.TrimRight(cfg.HubURL, "/")
	fmt.Printf(`Aestrl configured.

Identity: %s
Hub: %s

Your permanent MCP endpoint:
%s

ChatGPT / Custom MCP Client Configuration:
Option A (Bearer / API Key):
  - Server URL: %s
  - Authentication: Bearer Token / API Key
  - Token: %s

Option B (OAuth 2.1):
  - Discovery: %s/%s/.well-known/oauth-authorization-server
  - Authorization URL: %s/%s/oauth/authorize
  - Token URL: %s/%s/oauth/token
  - Dynamic Client Registration: %s/%s/oauth/register
  - Identity Authorization: Authorize with your MCP Token on the consent prompt

To share a directory, run:
  aestrl share ./project
`, cfg.Identity, cfg.HubURL, cfg.MCPEndpoint(), cfg.MCPEndpoint(), cfg.MCPToken,
		baseURL, cfg.Identity, baseURL, cfg.Identity, baseURL, cfg.Identity, baseURL, cfg.Identity)
}

func runHub(args []string) {
	fs := flag.NewFlagSet("hub", flag.ExitOnError)
	listen := fs.String("listen", ":8080", "Address and port to listen on")
	pubURL := fs.String("hub-url", "https://aestrl.dvrsfy.org", "Publicly reachable URL of this hub")
	dataDir := fs.String("data-dir", "./data", "Directory to store registered identities")
	_ = fs.Parse(args)

	cfg := hub.Config{
		ListenAddr: *listen,
		PublicURL:  *pubURL,
		DataDir:    *dataDir,
	}

	server, err := hub.New(cfg, web.FS())
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start hub: %v\n", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:    *listen,
		Handler: server.Handler(),
	}

	go func() {
		fmt.Printf("Aestrl hub listening on %s (Public URL: %s)\n", *listen, *pubURL)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "Hub error: %v\n", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down hub...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}

func runShare(args []string) {
	fs := flag.NewFlagSet("share", flag.ExitOnError)
	ttlStr := fs.String("ttl", "2h", "Session time-to-live (e.g. 2h, 30m, 1d)")
	_ = fs.Parse(args)

	dirArgs := fs.Args()
	if len(dirArgs) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: aestrl share <directory> [--ttl 2h]")
		os.Exit(1)
	}
	targetDir := dirArgs[0]

	ttl, err := time.ParseDuration(*ttlStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid TTL duration '%s': %v\n", *ttlStr, err)
		os.Exit(1)
	}

	cfg, err := config.Load("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "No existing configuration found. Running automatic setup...")
		runSetup([]string{})
		var loadErr error
		cfg, loadErr = config.Load("")
		if loadErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", loadErr)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := host.StartShare(ctx, cfg, targetDir, ttl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error starting share: %v\n", err)
		os.Exit(1)
	}
	defer sess.Close()

	absDir, _ := filepath.Abs(targetDir)
	fmt.Printf(`Aestrl sharing: %s

Share:
%s

MCP:
%s

Expires: %s
Press Ctrl+C to stop
`, absDir, sess.ShareURL, sess.MCPURL, *ttlStr)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-sigChan:
		fmt.Println("\nStopping share session...")
	case <-sess.Done():
		fmt.Println("\nShare session expired.")
	}
}

func runLs(args []string) {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "Output JSON")
	_ = fs.Parse(args)

	extra := fs.Args()
	var shareURL, path string
	if len(extra) >= 1 {
		shareURL = extra[0]
	}
	if len(extra) >= 2 {
		path = extra[1]
	}

	cli, err := client.ParseShareURL(shareURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	items, err := cli.ListDirectory(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list directory: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		data, _ := json.MarshalIndent(items, "", "  ")
		fmt.Println(string(data))
		return
	}

	fmt.Print(client.FormatListDirectory(items))
}

func runTree(args []string) {
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	depth := fs.Int("depth", 3, "Max tree depth")
	jsonOut := fs.Bool("json", false, "Output JSON")
	_ = fs.Parse(args)

	extra := fs.Args()
	var shareURL, path string
	if len(extra) >= 1 {
		shareURL = extra[0]
	}
	if len(extra) >= 2 {
		path = extra[1]
	}

	cli, err := client.ParseShareURL(shareURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	tree, err := cli.Tree(path, *depth)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get tree: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		data, _ := json.MarshalIndent(tree, "", "  ")
		fmt.Println(string(data))
		return
	}

	fmt.Print(client.RenderTree(tree, "", true))
}

func runRg(args []string) {
	fs := flag.NewFlagSet("rg", flag.ExitOnError)
	isRegex := fs.Bool("regex", false, "Interpret pattern as regex")
	jsonOut := fs.Bool("json", false, "Output JSON")
	_ = fs.Parse(args)

	extra := fs.Args()
	if len(extra) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: aestrl rg <share-url> <pattern> [path]")
		os.Exit(1)
	}

	shareURL := extra[0]
	pattern := extra[1]
	path := ""
	if len(extra) >= 3 {
		path = extra[2]
	}

	cli, err := client.ParseShareURL(shareURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	matches, err := cli.SearchContent(pattern, path, *isRegex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Search failed: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		data, _ := json.MarshalIndent(matches, "", "  ")
		fmt.Println(string(data))
		return
	}

	fmt.Print(client.FormatMatches(matches))
}

func runCat(args []string) {
	fs := flag.NewFlagSet("cat", flag.ExitOnError)
	lines := fs.String("lines", "", "Line range (e.g. 10:50)")
	raw := fs.Bool("raw", false, "Output raw content without line numbers")
	jsonOut := fs.Bool("json", false, "Output JSON")
	_ = fs.Parse(args)

	extra := fs.Args()
	if len(extra) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: aestrl cat <share-url> <path> [--lines M:N]")
		os.Exit(1)
	}

	shareURL := extra[0]
	path := extra[1]

	start, end, err := client.ParseLineRange(*lines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid line range: %v\n", err)
		os.Exit(1)
	}

	cli, err := client.ParseShareURL(shareURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	res, err := cli.ReadFile(path, start, end)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to read file: %v\n", err)
		os.Exit(1)
	}

	if *jsonOut {
		data, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(data))
		return
	}

	fmt.Print(client.FormatCat(res, !*raw))
}

func runGet(args []string) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	output := fs.String("o", "", "Destination file path")
	stdout := fs.Bool("stdout", false, "Write to standard output")
	_ = fs.Parse(args)

	extra := fs.Args()
	if len(extra) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: aestrl get <share-url> <path> [-o destination]")
		os.Exit(1)
	}

	shareURL := extra[0]
	path := extra[1]

	cli, err := client.ParseShareURL(shareURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	res, err := cli.ReadFile(path, 0, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Download failed: %v\n", err)
		os.Exit(1)
	}

	if *stdout {
		_, _ = io.WriteString(os.Stdout, res.Content)
		return
	}

	dest := *output
	if dest == "" {
		dest = filepath.Base(path)
	}

	if err := client.SaveFile(dest, []byte(res.Content)); err != nil {
		fmt.Fprintf(os.Stderr, "Failed saving file to %s: %v\n", dest, err)
		os.Exit(1)
	}

	fmt.Printf("Saved %s to %s (%d bytes)\n", path, dest, len(res.Content))
}
