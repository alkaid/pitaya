//go:build benchmark && stress

package client

import (
	"context"
	"errors"
)

// ConnectToStress is the bounded TCP-only connector used by the stress gate.
// It is build-tagged so the gate can cancel a stalled handshake without
// changing the production client's connection contract.
func (c *Client) ConnectToStress(ctx context.Context, address string) error {
	return c.connectToStress(ctx, address, nil)
}

func (c *Client) connectToStress(ctx context.Context, address string, afterHandshake func()) error {
	if ctx == nil {
		return errors.New("stress connect context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	_, err := c.connectToContext(ctx, "tcp://"+address, nil, afterHandshake)
	return err
}
