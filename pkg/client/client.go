package client

import (
	"context"

	"github.com/Encognitio/aestrl/pkg/p2p"
)

// Dial connects to a remote Aestrl share over encrypted WebRTC DataChannel.
func Dial(ctx context.Context, shareURL string) (*p2p.ClientSession, error) {
	return p2p.DialShare(ctx, shareURL)
}
