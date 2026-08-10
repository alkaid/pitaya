//go:build benchmark && stress

package client

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/alkaid/pitaya/v2/conn/message"
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

	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	c.conn = connection
	c.IncomingMsgChan = make(chan *message.Message, 10)
	c.closeChan = make(chan struct{})

	cancelComplete := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = connection.Close()
		close(cancelComplete)
	})
	handshakeErr := c.sendHandshakeRequest()
	heartbeat := 0
	if handshakeErr == nil {
		heartbeat, handshakeErr = c.receiveHandshakeResponse()
	}
	if handshakeErr == nil && afterHandshake != nil {
		afterHandshake()
	}
	if !stopCancel() {
		<-cancelComplete
	}
	if err := ctx.Err(); err != nil {
		close(c.closeChan)
		_ = connection.Close()
		return err
	}
	if handshakeErr != nil {
		close(c.closeChan)
		_ = connection.Close()
		return fmt.Errorf("stress client handshake: %w", handshakeErr)
	}
	c.startConnectionLoops(heartbeat)
	return nil
}
