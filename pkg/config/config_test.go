package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigDiscoveryAndIsolation(t *testing.T) {
	tempSharedDir := t.TempDir()

	// 1. Verify SaveShareState keeps credentials strictly OUTSIDE the shared directory
	sessionID := "share123"
	secret := "secret456"

	err := SaveShareState(tempSharedDir, sessionID, secret)
	if err != nil {
		t.Fatalf("SaveShareState failed: %v", err)
	}

	// Ensure no files or .aestrl directories were created inside the shared target directory!
	entries, err := os.ReadDir(tempSharedDir)
	if err != nil {
		t.Fatalf("failed to read shared dir: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("shared directory was modified! Found files/dirs inside targetDir: %v", names)
	}

	// 2. Verify DiscoverShareState successfully rediscovers the saved state from outside storage
	foundID, foundSecret := DiscoverShareState(tempSharedDir)
	if foundID != sessionID || foundSecret != secret {
		t.Fatalf("expected rediscovered (%q, %q), got (%q, %q)", sessionID, secret, foundID, foundSecret)
	}

	// 3. Verify sidecar config discovery priority
	cfg := &Config{
		Identity: "test-identity",
		HubURL:   "https://hub.example.com",
	}

	// Target directory sidecar
	sidecarPath := SidecarConfigPath(tempSharedDir)
	if err := os.MkdirAll(filepath.Dir(sidecarPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(sidecarPath); err != nil {
		t.Fatal(err)
	}

	discCfg, discPath, err := DiscoverConfig(tempSharedDir)
	if err != nil {
		t.Fatalf("DiscoverConfig failed: %v", err)
	}
	if discPath != sidecarPath || discCfg.Identity != "test-identity" {
		t.Fatalf("unexpected discovered config: path=%s, cfg=%+v", discPath, discCfg)
	}
}
