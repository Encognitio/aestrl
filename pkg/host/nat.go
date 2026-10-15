package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// PortMappingResult details the result of UPnP/NAT-PMP port mapping attempts.
type PortMappingResult struct {
	Success      bool
	ExternalIP   string
	ExternalPort int
	Diagnostic   string
}

// AttemptPortMapping attempts to automatically map an internal port using UPnP SSDP.
func AttemptPortMapping(ctx context.Context, internalPort int) PortMappingResult {
	if internalPort <= 0 {
		return PortMappingResult{
			Success:    false,
			Diagnostic: "Invalid internal port for mapping",
		}
	}

	// 1. Send SSDP M-SEARCH broadcast for InternetGatewayDevice
	ssdpAddr, err := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	if err != nil {
		return PortMappingResult{
			Success:    false,
			Diagnostic: fmt.Sprintf("Failed to resolve SSDP multicast: %v", err),
		}
	}

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return PortMappingResult{
			Success:    false,
			Diagnostic: fmt.Sprintf("Failed to bind UDP socket: %v", err),
		}
	}
	defer conn.Close()

	mSearch := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 1\r\n" +
		"ST: urn:schemas-upnp-org:service:WANIPConnection:1\r\n\r\n"

	_ = conn.SetDeadline(time.Now().Add(1200 * time.Millisecond))
	_, err = conn.WriteToUDP([]byte(mSearch), ssdpAddr)
	if err != nil {
		return PortMappingResult{
			Success:    false,
			Diagnostic: fmt.Sprintf("SSDP discovery write failed: %v", err),
		}
	}

	buf := make([]byte, 2048)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		return PortMappingResult{
			Success:    false,
			Diagnostic: "No UPnP/NAT-PMP gateway responded to SSDP multicast (likely behind CGNAT or router UPnP is disabled)",
		}
	}

	respStr := string(buf[:n])
	locRegex := regexp.MustCompile(`(?i)LOCATION:\s*([^\r\n]+)`)
	matches := locRegex.FindStringSubmatch(respStr)
	if len(matches) < 2 {
		return PortMappingResult{
			Success:    false,
			Diagnostic: "UPnP gateway responded without valid Location URL header",
		}
	}

	locURL := strings.TrimSpace(matches[1])
	controlURL, err := fetchControlURL(ctx, locURL)
	if err != nil {
		return PortMappingResult{
			Success:    false,
			Diagnostic: fmt.Sprintf("Failed retrieving UPnP description from %s: %v", locURL, err),
		}
	}

	// Request AddPortMapping SOAP action
	extIP, err := performAddPortMapping(ctx, controlURL, internalPort)
	if err != nil {
		return PortMappingResult{
			Success:    false,
			Diagnostic: fmt.Sprintf("UPnP AddPortMapping failed: %v", err),
		}
	}

	return PortMappingResult{
		Success:      true,
		ExternalIP:   extIP,
		ExternalPort: internalPort,
		Diagnostic:   fmt.Sprintf("Mapped external port %d via UPnP at %s", internalPort, extIP),
	}
}

func fetchControlURL(ctx context.Context, locURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", locURL, nil)
	if err != nil {
		return "", err
	}
	cli := &http.Client{Timeout: 2 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	controlRegex := regexp.MustCompile(`<controlURL>([^<]+)</controlURL>`)
	matches := controlRegex.FindStringSubmatch(string(body))
	if len(matches) < 2 {
		return "", fmt.Errorf("controlURL not found in device XML")
	}

	parsedLoc, err := url.Parse(locURL)
	if err != nil {
		return "", err
	}

	relControl := strings.TrimSpace(matches[1])
	parsedControl, err := parsedLoc.Parse(relControl)
	if err != nil {
		return "", err
	}

	return parsedControl.String(), nil
}

func performAddPortMapping(ctx context.Context, controlURL string, port int) (string, error) {
	soapBody := fmt.Sprintf(`<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body>
<u:AddPortMapping xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1">
  <NewRemoteHost></NewRemoteHost>
  <NewExternalPort>%d</NewExternalPort>
  <NewProtocol>TCP</NewProtocol>
  <NewInternalPort>%d</NewInternalPort>
  <NewInternalClient>%s</NewInternalClient>
  <NewEnabled>1</NewEnabled>
  <NewPortMappingDescription>Aestrl P2P</NewPortMappingDescription>
  <NewLeaseDuration>7200</NewLeaseDuration>
</u:AddPortMapping>
</s:Body>
</s:Envelope>`, port, port, getLocalIP())

	req, err := http.NewRequestWithContext(ctx, "POST", controlURL, bytes.NewReader([]byte(soapBody)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "text/xml; charset=\"utf-8\"")
	req.Header.Set("SOAPAction", "\"urn:schemas-upnp-org:service:WANIPConnection:1#AddPortMapping\"")

	cli := &http.Client{Timeout: 3 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("SOAP response error (%d): %s", resp.StatusCode, string(b))
	}

	return getLocalIP(), nil
}

func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}
