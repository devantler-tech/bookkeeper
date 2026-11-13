package numina

import (
	"context"
	"time"
)

// SetSleep replaces how the Client waits before a retry, so the external tests need not wait.
func (c *Client) SetSleep(sleep func(ctx context.Context, wait time.Duration) error) {
	c.sleep = sleep
}

// Sleep exposes the Client's default wait to the external tests.
func Sleep(ctx context.Context, wait time.Duration) error {
	return sleep(ctx, wait)
}
