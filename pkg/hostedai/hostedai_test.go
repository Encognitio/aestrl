//go:build !noai

package hostedai

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Encognitio/aestrl/pkg/engine"
)

func TestHostedAIServer(t *testing.T) {
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "sample.txt")
	content := "Line 1\nLine 2\nLine 3\n"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	eng, err := engine.New(tempDir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "testai123"
	secret := "aisecret456"

	server, pubURL, err := StartServer(eng, Config{Addr: "127.0.0.1:0", AllowHTTP: true}, sessionID, secret)
	if err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer server.Close()

	// 1. Test unauthorized access
	resp, err := http.Get(pubURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", resp.StatusCode)
	}

	// 2. Test authorized root HTML
	resp, err = http.Get(pubURL + "/?secret=" + secret)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Aestrl Inspection Endpoint") {
		t.Fatalf("expected HTML body, got: %s", string(body))
	}

	// 3. Test root JSON
	resp, err = http.Get(pubURL + "/?secret=" + secret + "&format=json")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	var rootData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&rootData); err != nil {
		t.Fatal(err)
	}
	if rootData["session_id"] != sessionID {
		t.Fatalf("unexpected root json: %v", rootData)
	}

	// 4. Test /read endpoint
	resp, err = http.Get(pubURL + "/read?path=sample.txt&secret=" + secret + "&format=json")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	var readRes engine.ReadFileResult
	if err := json.NewDecoder(resp.Body).Decode(&readRes); err != nil {
		t.Fatal(err)
	}
	if readRes.Content != strings.TrimRight(content, "\n") {
		t.Fatalf("expected %q, got %q", strings.TrimRight(content, "\n"), readRes.Content)
	}

	// 5. Test /raw Range request
	req, _ := http.NewRequest("GET", pubURL+"/raw?path=sample.txt&secret="+secret, nil)
	req.Header.Set("Range", "bytes=0-5")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 Partial Content, got %d", resp.StatusCode)
	}
	rangeBody, _ := io.ReadAll(resp.Body)
	if string(rangeBody) != "Line 1" {
		t.Fatalf("expected 'Line 1', got %q", string(rangeBody))
	}
}

func TestDirectIPCertificateAndHTTPS(t *testing.T) {
	tempDir := t.TempDir()
	certDir := filepath.Join(tempDir, "certs")
	_ = os.MkdirAll(certDir, 0700)

	testIP := net.ParseIP("127.0.0.2")
	_, err := GenerateTestIPCert(testIP, certDir)
	if err != nil {
		t.Fatalf("failed to generate test IP cert: %v", err)
	}

	eng, err := engine.New(tempDir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	sessionID := "testip123"
	secret := "ipsecret456"

	server, pubURL, err := StartServer(eng, Config{
		Addr:      "127.0.0.2:0",
		Domain:    "127.0.0.2",
		CertDir:   certDir,
		AllowHTTP: false,
	}, sessionID, secret)
	if err != nil {
		t.Fatalf("failed to start HTTPS server with IP cert: %v", err)
	}
	defer server.Close()

	if !strings.HasPrefix(pubURL, "https://127.0.0.2") {
		t.Fatalf("expected https URL, got: %s", pubURL)
	}

	// Verify TLS connection succeeds
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get(fmt.Sprintf("https://%s/?secret=%s&format=json", server.listener.Addr().String(), secret))
	if err != nil {
		t.Fatalf("HTTPS request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK over HTTPS, got: %d", resp.StatusCode)
	}
}

func TestDirectIPFallbackToHTTP(t *testing.T) {
	tempDir := t.TempDir()
	certDir := filepath.Join(tempDir, "empty-certs")

	eng, err := engine.New(tempDir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// 1. With AllowHTTP = true, failure to obtain Let's Encrypt cert should fall back to HTTP
	server, pubURL, err := StartServer(eng, Config{
		Addr:      "127.0.0.1:0",
		Domain:    "203.0.113.1", // Non-routable test IP
		CertDir:   certDir,
		AllowHTTP: true,
	}, "testfallback", "sec123")
	if err != nil {
		t.Fatalf("expected HTTP fallback to succeed, got error: %v", err)
	}
	defer server.Close()

	if !strings.HasPrefix(pubURL, "http://") {
		t.Fatalf("expected http fallback URL, got: %s", pubURL)
	}

	// 2. With AllowHTTP = false, failure to obtain cert must return clear error
	_, _, err = StartServer(eng, Config{
		Addr:      "127.0.0.1:0",
		Domain:    "203.0.113.1",
		CertDir:   certDir,
		AllowHTTP: false,
	}, "teststrict", "sec123")
	if err == nil {
		t.Fatal("expected error when HTTPS fails and AllowHTTP is false, got nil")
	}
	if !strings.Contains(err.Error(), "shortlived profile") || !strings.Contains(err.Error(), "--allow-http") {
		t.Fatalf("expected error mentioning shortlived profile and --allow-http, got: %v", err)
	}
}

