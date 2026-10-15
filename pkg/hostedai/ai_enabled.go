//go:build !noai

package hostedai

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Encognitio/aestrl/pkg/engine"
	"golang.org/x/crypto/acme/autocert"
)

const Enabled = true

// Config configures the Hosted-AI HTTP/HTTPS server.
type Config struct {
	Addr      string
	Domain    string
	AllowHTTP bool
	CertDir   string
}

// Server is a lightweight HTTP server exposing core inspection operations to Hosted-AI clients.
type Server struct {
	eng        *engine.Engine
	sessionID  string
	secretHash string
	server     *http.Server
	listener   net.Listener
	pubURL     string
}

// StartServer starts the Hosted-AI HTTP/HTTPS server.
func StartServer(eng *engine.Engine, cfg Config, sessionID, secret string) (*Server, string, error) {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}

	hash := sha256.Sum256([]byte(secret))
	secretHash := hex.EncodeToString(hash[:])

	var ln net.Listener
	var pubURL string

	// Attempt Let's Encrypt HTTPS if a domain is specified
	if cfg.Domain != "" && cfg.Domain != "localhost" && cfg.Domain != "127.0.0.1" {
		certDir := cfg.CertDir
		if certDir == "" {
			if home, err := os.UserHomeDir(); err == nil {
				certDir = filepath.Join(home, ".aestrl", "certs")
			} else {
				certDir = filepath.Join(os.TempDir(), "aestrl-certs")
			}
		}
		_ = os.MkdirAll(certDir, 0700)

		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Domain),
			Cache:      autocert.DirCache(certDir),
		}

		tlsConfig := m.TLSConfig()
		tlsLn, err := tls.Listen("tcp", cfg.Addr, tlsConfig)
		if err != nil {
			if !cfg.AllowHTTP {
				return nil, "", fmt.Errorf("Let's Encrypt HTTPS failed on %s: %w. Use --allow-http to explicitly approve unencrypted HTTP fallback", cfg.Domain, err)
			}
			// Explicit fallback approved
			rawLn, hErr := net.Listen("tcp", cfg.Addr)
			if hErr != nil {
				return nil, "", fmt.Errorf("failed to bind HTTP fallback on %s: %w", cfg.Addr, hErr)
			}
			ln = rawLn
			pubURL = fmt.Sprintf("http://%s", rawLn.Addr().String())
		} else {
			ln = tlsLn
			pubURL = fmt.Sprintf("https://%s", cfg.Domain)
		}
	} else {
		// Local or unencrypted HTTP
		rawLn, err := net.Listen("tcp", cfg.Addr)
		if err != nil {
			return nil, "", fmt.Errorf("failed to bind HTTP server on %s: %w", cfg.Addr, err)
		}
		ln = rawLn
		pubURL = fmt.Sprintf("http://%s", rawLn.Addr().String())
	}

	s := &Server{
		eng:        eng,
		sessionID:  sessionID,
		secretHash: secretHash,
		listener:   ln,
		pubURL:     pubURL,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.authMiddleware(s.handleRoot))
	mux.HandleFunc("/ls", s.authMiddleware(s.makeOpHandler("ls")))
	mux.HandleFunc("/tree", s.authMiddleware(s.makeOpHandler("tree")))
	mux.HandleFunc("/stat", s.authMiddleware(s.makeOpHandler("stat")))
	mux.HandleFunc("/find", s.authMiddleware(s.makeOpHandler("find")))
	mux.HandleFunc("/search", s.authMiddleware(s.makeOpHandler("search")))
	mux.HandleFunc("/grep", s.authMiddleware(s.makeOpHandler("grep")))
	mux.HandleFunc("/rg", s.authMiddleware(s.makeOpHandler("grep")))
	mux.HandleFunc("/context", s.authMiddleware(s.makeOpHandler("context")))
	mux.HandleFunc("/read", s.authMiddleware(s.makeOpHandler("read")))
	mux.HandleFunc("/cat", s.authMiddleware(s.makeOpHandler("read")))
	mux.HandleFunc("/outline", s.authMiddleware(s.makeOpHandler("outline")))
	mux.HandleFunc("/symbol", s.authMiddleware(s.makeOpHandler("symbol")))
	mux.HandleFunc("/refs", s.authMiddleware(s.makeOpHandler("refs")))
	mux.HandleFunc("/deps", s.authMiddleware(s.makeOpHandler("deps")))
	mux.HandleFunc("/compare", s.authMiddleware(s.makeOpHandler("compare")))
	mux.HandleFunc("/manifest", s.authMiddleware(s.makeOpHandler("manifest")))
	mux.HandleFunc("/raw", s.authMiddleware(s.handleRaw))

	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	go func() {
		_ = s.server.Serve(ln)
	}()

	return s, s.pubURL, nil
}

// URL returns the bound listener URL.
func (s *Server) URL() string {
	return s.pubURL
}

// Close stops the server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := r.URL.Query().Get("secret")
		if secret == "" {
			authHeader := r.Header.Get("Authorization")
			if strings.HasPrefix(authHeader, "Bearer ") {
				secret = strings.TrimPrefix(authHeader, "Bearer ")
			}
		}

		h := sha256.Sum256([]byte(secret))
		if hex.EncodeToString(h[:]) != s.secretHash {
			http.Error(w, "Unauthorized: missing or invalid ?secret= capability parameter", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) makeOpHandler(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		args := make(map[string]interface{})
		for k, v := range r.URL.Query() {
			if k == "secret" || k == "format" {
				continue
			}
			if len(v) == 1 {
				// Parse integers if applicable
				if intVal, err := strconv.Atoi(v[0]); err == nil {
					args[k] = intVal
				} else if boolVal, err := strconv.ParseBool(v[0]); err == nil {
					args[k] = boolVal
				} else {
					args[k] = v[0]
				}
			} else {
				args[k] = v
			}
		}

		res, err := s.eng.Execute(op, args)
		if err != nil {
			http.Error(w, fmt.Sprintf("Error executing %s: %v", op, err), http.StatusBadRequest)
			return
		}

		s.respond(w, r, op, res)
	}
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path parameter required", http.StatusBadRequest)
		return
	}

	offset := int64(0)
	length := int64(-1)

	if rangeHeader := r.Header.Get("Range"); strings.HasPrefix(rangeHeader, "bytes=") {
		parts := strings.Split(strings.TrimPrefix(rangeHeader, "bytes="), "-")
		if len(parts) >= 1 && parts[0] != "" {
			if start, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				offset = start
			}
		}
		if len(parts) >= 2 && parts[1] != "" {
			if end, err := strconv.ParseInt(parts[1], 10, 64); err == nil && end >= offset {
				length = (end - offset) + 1
			}
		}
	}

	rawRes, err := s.eng.GetRawFile(path, offset, length)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", rawRes.MimeType)
	w.Header().Set("Accept-Ranges", "bytes")
	if length > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(len(rawRes.Data))-1, rawRes.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(int64(len(rawRes.Data)), 10))
		w.WriteHeader(http.StatusOK)
	}
	_, _ = w.Write(rawRes.Data)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/info" {
		http.NotFound(w, r)
		return
	}

	secret := r.URL.Query().Get("secret")
	topItems, _ := s.eng.ListDirectory("")

	if isJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		data, _ := json.MarshalIndent(map[string]interface{}{
			"session_id":  s.sessionID,
			"description": "Aestrl Hosted-AI Read-Only P2P File Inspection API",
			"endpoints": []string{
				"/ls?path=<path>",
				"/tree?path=<path>&depth=<int>",
				"/stat?path=<path>",
				"/find?query=<query>",
				"/search?query=<query>",
				"/grep?query=<query>&is_regex=<bool>",
				"/context?query=<query>",
				"/read?path=<path>&start_line=<int>&end_line=<int>",
				"/outline?path=<path>",
				"/symbol?name=<symbol>",
				"/refs?name=<symbol>",
				"/deps?path=<path>",
				"/compare?path1=<p1>&path2=<p2>",
				"/manifest",
				"/raw?path=<path>",
			},
			"root_items": topItems,
		}, "", "  ")
		_, _ = w.Write(data)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><head><meta charset='utf-8'><title>Aestrl AI Inspection</title>")
	sb.WriteString("<style>body{font-family:sans-serif;margin:2rem;line-height:1.5;color:#222;} code,pre{background:#f4f4f5;padding:0.2rem 0.4rem;border-radius:4px;font-family:monospace;} a{color:#2563eb;text-decoration:none;} a:hover{text-decoration:underline;} .box{border:1px solid #e4e4e7;border-radius:6px;padding:1rem;margin:1rem 0;}</style>")
	sb.WriteString("</head><body>")
	sb.WriteString(fmt.Sprintf("<h1>Aestrl Inspection Endpoint: %s</h1>", html.EscapeString(s.sessionID)))
	sb.WriteString("<p>Self-describing read-only file exploration interface for AI agents.</p>")

	sb.WriteString("<div class='box'><h3>Quick Operations</h3><ul>")
	sb.WriteString(fmt.Sprintf("<li><a href='/context?query=main&secret=%s'>Explore Context (/context?query=main)</a></li>", secret))
	sb.WriteString(fmt.Sprintf("<li><a href='/tree?secret=%s'>Directory Tree (/tree)</a></li>", secret))
	sb.WriteString(fmt.Sprintf("<li><a href='/manifest?secret=%s'>Full Manifest (/manifest)</a></li>", secret))
	sb.WriteString("</ul></div>")

	sb.WriteString("<div class='box'><h3>Root Files</h3><ul>")
	for _, it := range topItems {
		if it.IsDir {
			sb.WriteString(fmt.Sprintf("<li>📁 <a href='/ls?path=%s&secret=%s'>%s/</a></li>", html.EscapeString(it.Path), secret, html.EscapeString(it.Name)))
		} else {
			sb.WriteString(fmt.Sprintf("<li>📄 <a href='/read?path=%s&secret=%s'>%s</a> (<em>%d bytes</em>)</li>", html.EscapeString(it.Path), secret, html.EscapeString(it.Name), it.Size))
		}
	}
	sb.WriteString("</ul></div>")

	sb.WriteString("</body></html>")
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) respond(w http.ResponseWriter, r *http.Request, op string, res interface{}) {
	if isJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		data, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(data)
		return
	}

	secret := r.URL.Query().Get("secret")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><head><meta charset='utf-8'><title>Aestrl " + html.EscapeString(op) + "</title>")
	sb.WriteString("<style>body{font-family:sans-serif;margin:2rem;line-height:1.5;color:#222;} pre{background:#f4f4f5;padding:1rem;border-radius:6px;overflow-x:auto;font-family:monospace;font-size:0.9rem;} a{color:#2563eb;text-decoration:none;} a:hover{text-decoration:underline;} .nav{margin-bottom:1rem;}</style>")
	sb.WriteString("</head><body>")
	sb.WriteString(fmt.Sprintf("<div class='nav'><a href='/?secret=%s'>← Back to Root</a> | <a href='%s&format=json'>JSON Format</a></div>", secret, html.EscapeString(r.RequestURI)))
	sb.WriteString(fmt.Sprintf("<h2>Operation: %s</h2>", html.EscapeString(op)))

	switch v := res.(type) {
	case *engine.ReadFileResult:
		sb.WriteString(fmt.Sprintf("<p><strong>File:</strong> %s (Lines %d-%d of %d)</p>", html.EscapeString(v.Path), v.StartLine, v.EndLine, v.TotalLines))
		sb.WriteString("<pre><code>" + html.EscapeString(v.Content) + "</code></pre>")
	default:
		data, _ := json.MarshalIndent(v, "", "  ")
		sb.WriteString("<pre><code>" + html.EscapeString(string(data)) + "</code></pre>")
	}

	sb.WriteString("</body></html>")
	_, _ = w.Write([]byte(sb.String()))
}

func isJSON(r *http.Request) bool {
	return r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json")
}
