package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Encognitio/aestrl/pkg/engine"
	"github.com/Encognitio/aestrl/pkg/hub"
)

// Client interacts with remote Aestrl shares.
type Client struct {
	HubURL    string
	SessionID string
	Secret    string
	HTTP      *http.Client
}

// ParseShareURL parses a full Aestrl share URL into a Client.
func ParseShareURL(rawURL string) (*Client, error) {
	if rawURL == "" {
		rawURL = os.Getenv("AESTRL_SHARE")
	}
	if rawURL == "" {
		return nil, errors.New("share URL required (pass as argument or set AESTRL_SHARE)")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid share URL: %w", err)
	}

	pathParts := strings.Split(strings.Trim(u.Path, "/"), "/")
	var sessionID string
	for i, part := range pathParts {
		if part == "s" && i+1 < len(pathParts) {
			sessionID = pathParts[i+1]
			break
		}
	}
	if sessionID == "" {
		return nil, fmt.Errorf("unable to find session ID in URL: %s", rawURL)
	}

	secret := u.Fragment
	if secret == "" {
		secret = u.Query().Get("secret")
	}
	if secret == "" {
		return nil, errors.New("share secret capability missing in URL (must include #<secret>)")
	}

	hubURL := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	if u.Scheme == "" {
		hubURL = "https://" + u.Host
	}

	return &Client{
		HubURL:    hubURL,
		SessionID: sessionID,
		Secret:    secret,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

func (c *Client) callInspect(op string, args map[string]interface{}) (json.RawMessage, error) {
	endpoint := fmt.Sprintf("%s/api/inspect", strings.TrimRight(c.HubURL, "/"))
	reqBody := hub.InspectRequest{
		SessionID: c.SessionID,
		Secret:    c.Secret,
		Op:        op,
		Args:      args,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTP.Post(endpoint, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("request to hub failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var inspectResp hub.InspectResponse
	if err := json.Unmarshal(body, &inspectResp); err != nil {
		return nil, fmt.Errorf("invalid hub response: %w", err)
	}

	if inspectResp.Error != "" {
		return nil, errors.New(inspectResp.Error)
	}

	return inspectResp.Result, nil
}

// ListDirectory lists files in path.
func (c *Client) ListDirectory(path string) ([]engine.FileItem, error) {
	raw, err := c.callInspect("list_directory", map[string]interface{}{"path": path})
	if err != nil {
		return nil, err
	}
	var items []engine.FileItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// Tree gets directory tree.
func (c *Client) Tree(path string, depth int) (*engine.TreeNode, error) {
	raw, err := c.callInspect("tree", map[string]interface{}{"path": path, "depth": depth})
	if err != nil {
		return nil, err
	}
	var node engine.TreeNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return nil, err
	}
	return &node, nil
}

// SearchContent runs regex or literal search.
func (c *Client) SearchContent(query, path string, isRegex bool) ([]engine.SearchMatch, error) {
	raw, err := c.callInspect("search_content", map[string]interface{}{
		"query":    query,
		"path":     path,
		"is_regex": isRegex,
	})
	if err != nil {
		return nil, err
	}
	var matches []engine.SearchMatch
	if err := json.Unmarshal(raw, &matches); err != nil {
		return nil, err
	}
	return matches, nil
}

// ReadFile reads lines from a remote file.
func (c *Client) ReadFile(path string, startLine, endLine int) (*engine.ReadFileResult, error) {
	raw, err := c.callInspect("read_file", map[string]interface{}{
		"path":       path,
		"start_line": startLine,
		"end_line":   endLine,
	})
	if err != nil {
		return nil, err
	}
	var res engine.ReadFileResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// FileInfo gets metadata for a remote path.
func (c *Client) FileInfo(path string) (*engine.FileItem, error) {
	raw, err := c.callInspect("file_info", map[string]interface{}{"path": path})
	if err != nil {
		return nil, err
	}
	var item engine.FileItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// Formatters for terminal output

// FormatListDirectory formats file items into clean terminal table.
func FormatListDirectory(items []engine.FileItem) string {
	if len(items) == 0 {
		return "(empty directory)\n"
	}

	var sb strings.Builder
	for _, it := range items {
		typeMarker := " "
		if it.IsDir {
			typeMarker = "d"
		}
		sizeStr := formatBytes(it.Size)
		if it.IsDir {
			sizeStr = "-"
		}
		sb.WriteString(fmt.Sprintf("%s  %-10s  %-20s  %s\n",
			typeMarker,
			sizeStr,
			it.ModTime.Format("2006-01-02 15:04"),
			it.Name,
		))
	}
	return sb.String()
}

// RenderTree renders a TreeNode into ASCII tree lines.
func RenderTree(node *engine.TreeNode, prefix string, isLast bool) string {
	if node == nil {
		return ""
	}
	var sb strings.Builder
	marker := "├── "
	if isLast {
		marker = "└── "
	}
	if prefix == "" {
		sb.WriteString(node.Name + "\n")
	} else {
		sb.WriteString(prefix + marker + node.Name + "\n")
	}

	childPrefix := prefix
	if prefix != "" {
		if isLast {
			childPrefix += "    "
		} else {
			childPrefix += "│   "
		}
	}

	for i, ch := range node.Children {
		last := i == len(node.Children)-1
		sb.WriteString(RenderTree(ch, childPrefix, last))
	}
	return sb.String()
}

// FormatMatches formats ripgrep-style content search matches.
func FormatMatches(matches []engine.SearchMatch) string {
	if len(matches) == 0 {
		return "No matches found.\n"
	}
	var sb strings.Builder
	for _, m := range matches {
		sb.WriteString(fmt.Sprintf("\033[35m%s\033[0m:\033[32m%d\033[0m: %s\n", m.Path, m.LineNumber, m.LineText))
	}
	return sb.String()
}

// FormatCat formats read file lines with line numbering.
func FormatCat(res *engine.ReadFileResult, withLineNumbers bool) string {
	if res == nil {
		return ""
	}
	if res.IsBinary {
		return fmt.Sprintf("[Binary file: %s (%s, %s)]\n", res.Path, res.MimeType, formatBytes(res.Size))
	}

	if !withLineNumbers {
		return res.Content + "\n"
	}

	lines := strings.Split(res.Content, "\n")
	var sb strings.Builder
	start := res.StartLine
	if start <= 0 {
		start = 1
	}
	for i, line := range lines {
		sb.WriteString(fmt.Sprintf("%5d | %s\n", start+i, line))
	}
	return sb.String()
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// ParseLineRange parses "50:100" into start and end integers.
func ParseLineRange(rangeStr string) (int, int, error) {
	if rangeStr == "" {
		return 0, 0, nil
	}
	parts := strings.Split(rangeStr, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid line range format '%s', expected START:END (e.g. 10:50)", rangeStr)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

// SaveFile saves file content to local disk safely.
func SaveFile(destPath string, content []byte) error {
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, content, 0644)
}
