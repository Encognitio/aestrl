//go:build !noai

package hostedai

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/acme"
)

// IPCertManager manages TLS certificates for direct public IP addresses,
// supporting Let's Encrypt's shortlived certificate profile.
type IPCertManager struct {
	IP             net.IP
	CertDir        string
	ACMEDirectory  string
	Profile        string // e.g. "shortlived"
	AccountKeyPath string
}

// NewIPCertManager creates a manager for public IP certificates.
func NewIPCertManager(ip net.IP, certDir string) *IPCertManager {
	if certDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			certDir = filepath.Join(home, ".aestrl", "certs")
		} else {
			certDir = filepath.Join(os.TempDir(), "aestrl-certs")
		}
	}
	_ = os.MkdirAll(certDir, 0700)
	return &IPCertManager{
		IP:             ip,
		CertDir:        certDir,
		ACMEDirectory:  acme.LetsEncryptURL,
		Profile:        "shortlived",
		AccountKeyPath: filepath.Join(certDir, "acme_account.key"),
	}
}

// CertPaths returns the paths to the certificate and private key files.
func (m *IPCertManager) CertPaths() (string, string) {
	safeIP := m.IP.String()
	return filepath.Join(m.CertDir, safeIP+".crt"), filepath.Join(m.CertDir, safeIP+".key")
}

// LoadCachedCert loads an existing, unexpired certificate from the cache directory.
func (m *IPCertManager) LoadCachedCert() (*tls.Certificate, error) {
	certPath, keyPath := m.CertPaths()
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}

	if len(cert.Certificate) > 0 {
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		if err == nil {
			// Check expiration (shortlived certificates have a 6-day lifespan)
			if time.Now().After(parsed.NotAfter) {
				return nil, errors.New("cached IP certificate expired")
			}
		}
	}

	return &cert, nil
}

// ObtainCert attempts to load an existing certificate or request a new one
// using Let's Encrypt's shortlived ACME profile for IP identifiers.
func (m *IPCertManager) ObtainCert(ctx context.Context) (*tls.Certificate, error) {
	// 1. Try cached certificate
	if cert, err := m.LoadCachedCert(); err == nil {
		return cert, nil
	}

	// 2. Request new certificate via ACME with shortlived profile
	client := &acme.Client{
		DirectoryURL: m.ACMEDirectory,
	}

	// Initialize account key
	accountKey, err := m.getOrCreateAccountKey()
	if err != nil {
		return nil, fmt.Errorf("failed to load/generate ACME account key: %w", err)
	}
	client.Key = accountKey

	// Register account with TOS agreement
	_, _ = client.Register(ctx, &acme.Account{}, acme.AcceptTOS)

	// Create IP identifier order
	ids := []acme.AuthzID{{Type: "ip", Value: m.IP.String()}}
	order, err := client.AuthorizeOrder(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("ACME order creation failed for IP %s (profile: %s): %w", m.IP, m.Profile, err)
	}

	// Complete authorizations
	for _, authzURL := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, authzURL)
		if err != nil {
			return nil, fmt.Errorf("failed to get authorization %s: %w", authzURL, err)
		}
		if authz.Status == acme.StatusValid {
			continue
		}

		// Find supported challenge (http-01 or tls-alpn-01 for IP certs)
		var chal *acme.Challenge
		for _, c := range authz.Challenges {
			if c.Type == "http-01" || c.Type == "tls-alpn-01" {
				chal = c
				break
			}
		}
		if chal == nil {
			return nil, fmt.Errorf("no supported challenge (http-01 or tls-alpn-01) for IP %s", m.IP)
		}

		if _, err := client.Accept(ctx, chal); err != nil {
			return nil, fmt.Errorf("failed to accept challenge: %w", err)
		}

		if _, err := client.WaitAuthorization(ctx, authzURL); err != nil {
			return nil, fmt.Errorf("authorization challenge failed for IP %s: %w", m.IP, err)
		}
	}

	// Wait for order to become ready
	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("order waiting failed: %w", err)
	}

	// Generate certificate key and CSR with IP SAN
	certKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate cert key: %w", err)
	}

	csrTemplate := &x509.CertificateRequest{
		IPAddresses: []net.IP{m.IP},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, certKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create CSR: %w", err)
	}

	// Finalize order
	derCerts, certURL, err := client.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return nil, fmt.Errorf("failed to finalize certificate order: %w", err)
	}
	if len(derCerts) == 0 && certURL != "" {
		derCerts, err = client.FetchCert(ctx, certURL, true)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch issued certificate: %w", err)
		}
	}

	// Save certificate and key to disk
	certPath, keyPath := m.CertPaths()
	var certPEM []byte
	for _, b := range derCerts {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})...)
	}
	keyBytes, err := x509.MarshalECPrivateKey(certKey)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	_ = os.WriteFile(certPath, certPEM, 0600)
	_ = os.WriteFile(keyPath, keyPEM, 0600)

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

func (m *IPCertManager) getOrCreateAccountKey() (*ecdsa.PrivateKey, error) {
	if data, err := os.ReadFile(m.AccountKeyPath); err == nil {
		block, _ := pem.Decode(data)
		if block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return k, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err == nil {
		pemData := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
		_ = os.WriteFile(m.AccountKeyPath, pemData, 0600)
	}
	return key, nil
}

// GenerateTestIPCert creates a self-signed IP certificate for testing direct IP TLS.
func GenerateTestIPCert(ip net.IP, certDir string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject: pkix.Name{
			CommonName: ip.String(),
		},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(6 * 24 * time.Hour), // 6 days (shortlived)
		IPAddresses:           []net.IP{ip},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes})
	keyBytes, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})

	if certDir != "" {
		_ = os.MkdirAll(certDir, 0700)
		_ = os.WriteFile(filepath.Join(certDir, ip.String()+".crt"), certPEM, 0600)
		_ = os.WriteFile(filepath.Join(certDir, ip.String()+".key"), keyPEM, 0600)
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}
