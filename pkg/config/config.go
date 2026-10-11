package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrConfigNotFound = errors.New("aestrl configuration not found; run 'aestrl setup' first")
)

// Config represents persistent identity and routing credentials.
type Config struct {
	Identity   string    `json:"identity"`
	HubURL     string    `json:"hub_url"`
	HostToken  string    `json:"host_token"`
	MCPToken   string    `json:"mcp_token"`
	DefaultTTL string    `json:"default_ttl,omitempty"`
	Exclusions []string  `json:"exclusions,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// DefaultHubURL is the default public coordinator address.
const DefaultHubURL = "https://aestrl.dvrsfy.org"

// DefaultConfigPath returns the canonical platform-specific configuration path.
func DefaultConfigPath() (string, error) {
	if envPath := os.Getenv("AESTRL_CONFIG"); envPath != "" {
		return envPath, nil
	}
	baseDir, err := os.UserConfigDir()
	if err != nil {
		// Fallback to home directory
		home, hErr := os.UserHomeDir()
		if hErr != nil {
			return "", fmt.Errorf("unable to determine user config directory: %w", err)
		}
		baseDir = filepath.Join(home, ".config")
	}
	return filepath.Join(baseDir, "aestrl", "config.json"), nil
}

// GenerateIdentityID generates a cryptographically random, clean alphanumeric identity ID (e.g. 9 chars: "h7af8ven8").
func GenerateIdentityID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	length := 9
	result := make([]byte, length)
	for i := range result {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			result[i] = charset[i%len(charset)]
		} else {
			result[i] = charset[n.Int64()]
		}
	}
	return string(result)
}

// GenerateSecureToken returns a cryptographically secure hex-encoded token of n random bytes.
func GenerateSecureToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto rand failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Load loads the configuration from the specified path, or DefaultConfigPath if empty.
func Load(customPath string) (*Config, error) {
	path := customPath
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return nil, err
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config JSON: %w", err)
	}

	if cfg.HubURL == "" {
		cfg.HubURL = DefaultHubURL
	}
	cfg.HubURL = strings.TrimRight(cfg.HubURL, "/")

	return &cfg, nil
}

// Save writes the configuration safely to disk with 0600 permissions.
func (c *Config) Save(customPath string) error {
	path := customPath
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return err
		}
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}

	// Write with tight 0600 permissions
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}
	return nil
}

// MCPEndpoint returns the full persistent MCP endpoint URL.
func (c *Config) MCPEndpoint() string {
	return fmt.Sprintf("%s/%s/mcp", strings.TrimRight(c.HubURL, "/"), c.Identity)
}

// HostWSEndpoint returns the WebSocket endpoint URL for host connection.
func (c *Config) HostWSEndpoint() string {
	hub := strings.TrimRight(c.HubURL, "/")
	wsScheme := "ws"
	if strings.HasPrefix(hub, "https://") {
		wsScheme = "wss"
		hub = strings.TrimPrefix(hub, "https://")
	} else if strings.HasPrefix(hub, "http://") {
		hub = strings.TrimPrefix(hub, "http://")
	}
	return fmt.Sprintf("%s://%s/%s/host/ws", wsScheme, hub, c.Identity)
}

// Export encodes the configuration into a compact base64 string for secure transfer.
func (c *Config) Export() (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// Import parses an exported base64 configuration string.
func Import(encoded string) (*Config, error) {
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid exported string: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config JSON: %w", err)
	}
	return &cfg, nil
}
