//go:build noai

package hostedai

import (
	"errors"

	"github.com/Encognitio/aestrl/pkg/engine"
)

const Enabled = false

// Config stub for -tags noai.
type Config struct {
	Addr      string
	Domain    string
	AllowHTTP bool
	CertDir   string
}

// Server stub when compiled with -tags noai.
type Server struct{}

func (s *Server) Close() error {
	return nil
}

func (s *Server) URL() string {
	return ""
}

// StartServer returns an error indicating AI adapter is excluded.
func StartServer(eng *engine.Engine, cfg Config, sessionID, secret string) (*Server, string, error) {
	return nil, "", errors.New("Hosted-AI adapter excluded in this build (-tags noai)")
}
