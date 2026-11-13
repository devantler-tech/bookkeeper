package mail

import "context"

// CapabilityMail exposes the JMAP mail capability to the external tests.
const CapabilityMail = capabilityMail

// AccountID exposes the account Connect selected to the external tests.
func (c *Client) AccountID() string {
	return c.accountID
}

// Call exposes the guarded JMAP method call to the external tests.
func (c *Client) Call(ctx context.Context, method string, args map[string]any, out any) error {
	return c.call(ctx, method, args, out)
}
