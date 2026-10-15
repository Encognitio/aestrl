package host

import (
	"context"
	"time"
)

// StartShare is a convenience function that starts a foreground share session without the background daemon.
func StartShare(ctx context.Context, path string, opts ShareOptions) (*ActiveShare, error) {
	share, err := NewActiveShare(path, opts)
	if err != nil {
		return nil, err
	}

	go func() {
		select {
		case <-ctx.Done():
			share.Close()
		case <-time.After(time.Until(share.Info.ExpiresAt)):
			share.Close()
		}
	}()

	return share, nil
}
