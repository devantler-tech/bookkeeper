package numina

import "time"

// SetSleep replaces how the Client waits before a retry, so the external tests need not wait.
func (c *Client) SetSleep(sleep func(time.Duration)) {
	c.sleep = sleep
}
