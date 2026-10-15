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
	HostToken  string    `json:"host_token,omitempty"`
	DefaultTTL string    `json:"default_ttl,omitempty"`
	AllowHTTP  bool      `json:"allow_http,omitempty"`
	Exclusions []string  `json:"exclusions,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitempty"`
}

// ShareState represents persistent reusable share identity for a directory.
type ShareState struct {
	SessionID string    `json:"session_id"`
	Secret    string    `json:"secret"`
	UpdatedAt time.Time `json:"updated_at"`
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
		home, hErr := os.UserHomeDir()
		if hErr != nil {
			return "", fmt.Errorf("unable to determine user config directory: %w", err)
		}
		baseDir = filepath.Join(home, ".config")
	}
	return filepath.Join(baseDir, "aestrl", "config.json"), nil
}

// SidecarConfigPath returns the path to a directory's local .aestrl/config.json.
func SidecarConfigPath(dir string) string {
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, ".aestrl", "config.json")
}

// DiscoverConfig discovers configuration using the priority:
// 1. Target directory sidecar (.aestrl/config.json)
// 2. Current working directory sidecar (.aestrl/config.json)
// 3. AESTRL_CONFIG environment variable
// 4. OS user config (~/.config/aestrl/config.json)
func DiscoverConfig(targetDir string) (*Config, string, error) {
	// 1. Target dir sidecar
	if targetDir != "" {
		sidecar := SidecarConfigPath(targetDir)
		if cfg, err := Load(sidecar); err == nil {
			return cfg, sidecar, nil
		}
	}

	// 2. Current directory sidecar
	cwdSidecar := SidecarConfigPath(".")
	if cfg, err := Load(cwdSidecar); err == nil {
		return cfg, cwdSidecar, nil
	}

	// 3. Environment or OS default
	defPath, err := DefaultConfigPath()
	if err != nil {
		return nil, "", err
	}
	if cfg, err := Load(defPath); err == nil {
		return cfg, defPath, nil
	}

	return nil, defPath, ErrConfigNotFound
}

// DiscoverShareState checks targetDir for existing reusable .aestrl/share.json.
func DiscoverShareState(targetDir string) (string, string) {
	if targetDir == "" {
		targetDir = "."
	}
	stateFile := filepath.Join(targetDir, ".aestrl", "share.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return "", ""
	}
	var state ShareState
	if err := json.Unmarshal(data, &state); err == nil && state.SessionID != "" && state.Secret != "" {
		return state.SessionID, state.Secret
	}
	return "", ""
}

// SaveShareState saves reusable share session credentials to .aestrl/share.json.
func SaveShareState(targetDir, sessionID, secret string) error {
	if targetDir == "" {
		targetDir = "."
	}
	dir := filepath.Join(targetDir, ".aestrl")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	state := ShareState{
		SessionID: sessionID,
		Secret:    secret,
		UpdatedAt: time.Now(),
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "share.json"), data, 0600)
}

// DefaultConfig generates a new default configuration instance.
func DefaultConfig() (*Config, error) {
	token, err := GenerateSecureToken(16)
	if err != nil {
		return nil, err
	}
	return &Config{
		Identity:   GenerateIdentityID(),
		HubURL:     DefaultHubURL,
		HostToken:  token,
		DefaultTTL: "2h",
		AllowHTTP:  true,
		CreatedAt:  time.Now(),
	}, nil
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
