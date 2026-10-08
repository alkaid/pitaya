// Copyright (c) TFG Co. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/alkaid/pitaya/v2/logger"

	"github.com/alkaid/goerrors/apierrors"
	"go.uber.org/zap"

	"github.com/alkaid/pitaya/v2/acceptor"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/conn/packet"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/alkaid/pitaya/v2/util/compression"
	"github.com/gorilla/websocket"
)

// HandshakeSys struct
type HandshakeSys struct {
	Dict       map[string]uint16 `json:"dict"`
	Heartbeat  int               `json:"heartbeat"`
	Serializer string            `json:"serializer"`
}

// HandshakeData struct
type HandshakeData struct {
	Code int          `json:"code"`
	Sys  HandshakeSys `json:"sys"`
}

type pendingRequest struct {
	msg    *message.Message
	sentAt time.Time
}

var clientActivityEpoch = time.Now()

const (
	defaultClientIOTimeout = 5 * time.Second
	clientInflightLimit    = 30
)

var (
	errClientAlreadyConnected = errors.New("client: already connected")
	errClientConnecting       = errors.New("client: connection in progress")
	errClientConnectionClosed = errors.New("client: connection closed")
)

type clientConnectionState uint8

const (
	clientStateIdle clientConnectionState = iota
	clientStateConnecting
	clientStateConnected
)

type connectAttempt struct {
	ctx                   context.Context
	cancel                context.CancelFunc
	done                  sync.WaitGroup
	conn                  net.Conn
	cancelledByDisconnect bool
	handshakeData         *session.HandshakeData
	packetEncoder         codec.PacketEncoder
	messageEncoder        message.Encoder
}

type connectionGeneration struct {
	conn         net.Conn
	closeCh      chan struct{}
	closeOnce    sync.Once
	packetChan   chan *packet.Packet
	incomingChan chan *message.Message

	pendingMu sync.Mutex
	pending   map[uint]*pendingRequest
	slots     chan struct{}
	closed    bool

	writeMu          sync.Mutex
	lastActivityTick atomic.Int64
	packetEncoder    codec.PacketEncoder
	messageEncoder   message.Encoder

	loops sync.WaitGroup
}

// Client struct
type Client struct {
	Connected       bool
	IncomingMsgChan chan *message.Message

	packetEncoder       codec.PacketEncoder
	messageEncoder      message.Encoder
	requestTimeout      time.Duration
	handshakeTimeout    time.Duration
	nextID              uint32
	clientHandshakeData *session.HandshakeData
	onDisconnected      func(reason CloseReason)

	connMutex sync.Mutex
	state     clientConnectionState
	attempt   *connectAttempt
	current   *connectionGeneration
}

// MsgChannel return the incoming message channel
func (c *Client) MsgChannel() chan *message.Message {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	return c.IncomingMsgChan
}

// ConnectedStatus return the connection status
func (c *Client) ConnectedStatus() bool {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	return c.Connected
}

// New returns a new client
func New(requestTimeout ...time.Duration) *Client {
	reqTimeout := 5 * time.Second
	if len(requestTimeout) > 0 {
		reqTimeout = requestTimeout[0]
	}

	return &Client{
		Connected:        false,
		state:            clientStateIdle,
		packetEncoder:    codec.NewPomeloPacketEncoder(),
		requestTimeout:   reqTimeout,
		handshakeTimeout: defaultClientIOTimeout,
		messageEncoder:   message.NewMessagesEncoder(false),
		clientHandshakeData: &session.HandshakeData{
			Sys: session.HandshakeClientData{
				Platform:    "mac",
				LibVersion:  "0.3.5-release",
				BuildNumber: "20",
				Version:     "2.1",
			},
			User: map[string]interface{}{
				"age": 30,
			},
		},
	}
}

// SetClientHandshakeData sets the data to send inside handshake
func (c *Client) SetClientHandshakeData(data *session.HandshakeData) {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	c.clientHandshakeData = data
}

func (c *Client) sendHandshakeRequestTo(conn net.Conn, handshakeData *session.HandshakeData, packetEncoder codec.PacketEncoder) error {
	if conn == nil {
		return errClientConnectionClosed
	}
	enc, err := json.Marshal(handshakeData)
	if err != nil {
		return err
	}

	p, err := packetEncoder.Encode(packet.Handshake, enc)
	if err != nil {
		return err
	}

	_, err = writeConnWithTimeout(conn, p, c.handshakeTimeout)
	return err
}

func (c *Client) receiveHandshakeResponseFrom(conn net.Conn, packetEncoder codec.PacketEncoder) (int, error) {
	if conn == nil {
		return 0, errClientConnectionClosed
	}
	handshakePacket, err := readPacket(conn)
	if err != nil {
		return 0, err
	}

	if handshakePacket.Type != packet.Handshake {
		return 0, fmt.Errorf("got first packet from server that is not a handshake, aborting")
	}

	handshake := &HandshakeData{}
	if compression.IsCompressed(handshakePacket.Data) {
		handshakePacket.Data, err = compression.InflateData(handshakePacket.Data)
		if err != nil {
			return 0, err
		}
	}

	err = json.Unmarshal(handshakePacket.Data, handshake)
	if err != nil {
		return 0, err
	}

	Log.Debug("got handshake from sv", zap.Any("data", handshake))

	if handshake.Sys.Dict != nil {
		message.SetDictionary(handshake.Sys.Dict)
	}
	p, err := packetEncoder.Encode(packet.HandshakeAck, []byte{})
	if err != nil {
		return 0, err
	}
	_, err = writeConnWithTimeout(conn, p, c.handshakeTimeout)
	if err != nil {
		return 0, err
	}
	return handshake.Sys.Heartbeat, nil
}

func writeConnWithTimeout(conn net.Conn, b []byte, timeout time.Duration) (int, error) {
	if conn == nil {
		return 0, errClientConnectionClosed
	}
	err := conn.SetWriteDeadline(time.Now().Add(timeout))
	if err != nil {
		return 0, err
	}
	n, err := conn.Write(b)
	if err != nil {
		return n, err
	}
	if n != len(b) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func newConnectionGeneration(
	conn net.Conn,
	packetEncoder codec.PacketEncoder,
	messageEncoder message.Encoder,
) *connectionGeneration {
	gen := &connectionGeneration{
		conn:           conn,
		closeCh:        make(chan struct{}),
		packetChan:     make(chan *packet.Packet, 10),
		incomingChan:   make(chan *message.Message, 10),
		pending:        make(map[uint]*pendingRequest),
		slots:          make(chan struct{}, clientInflightLimit),
		packetEncoder:  packetEncoder,
		messageEncoder: messageEncoder,
	}
	gen.recordServerActivity(time.Now())
	return gen
}

func (c *Client) startGenerationLoops(gen *connectionGeneration, heartbeat int) {
	loopCount := 3
	if heartbeat > 0 {
		loopCount++
	}
	gen.loops.Add(loopCount)

	if heartbeat > 0 {
		go func() {
			defer gen.loops.Done()
			c.sendHeartbeats(gen, heartbeat)
		}()
	}
	go func() {
		defer gen.loops.Done()
		c.handleServerMessages(gen)
	}()
	go func() {
		defer gen.loops.Done()
		c.handlePackets(gen)
	}()
	go func() {
		defer gen.loops.Done()
		c.pendingRequestsReaper(gen)
	}()
}

func (gen *connectionGeneration) safeWrite(b []byte, timeout time.Duration) (int, error) {
	select {
	case <-gen.closeCh:
		return 0, errClientConnectionClosed
	default:
	}
	gen.writeMu.Lock()
	defer gen.writeMu.Unlock()
	select {
	case <-gen.closeCh:
		return 0, errClientConnectionClosed
	default:
	}
	return writeConnWithTimeout(gen.conn, b, timeout)
}

func (gen *connectionGeneration) takePending(id uint) (*pendingRequest, bool) {
	gen.pendingMu.Lock()
	defer gen.pendingMu.Unlock()
	pending, ok := gen.pending[id]
	if !ok {
		return nil, false
	}
	delete(gen.pending, id)
	<-gen.slots
	return pending, true
}

func (gen *connectionGeneration) takeExpiredPending(now time.Time, timeout time.Duration) []*pendingRequest {
	gen.pendingMu.Lock()
	defer gen.pendingMu.Unlock()
	expired := make([]*pendingRequest, 0)
	for id, pending := range gen.pending {
		if pending.sentAt.IsZero() || now.Sub(pending.sentAt) <= timeout {
			continue
		}
		expired = append(expired, pending)
		delete(gen.pending, id)
		<-gen.slots
	}
	return expired
}

func (gen *connectionGeneration) deliver(msg *message.Message) bool {
	select {
	case gen.incomingChan <- msg:
		return true
	case <-gen.closeCh:
		return false
	}
}

// pendingRequestsReaper deletes timed out requests.
func (c *Client) pendingRequestsReaper(gen *connectionGeneration) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !c.reapPending(gen, time.Now()) {
				return
			}
		case <-gen.closeCh:
			return
		}
	}
}

func (c *Client) reapPending(gen *connectionGeneration, now time.Time) bool {
	for _, pendingReq := range gen.takeExpiredPending(now, c.requestTimeout) {
		err := apierrors.GatewayTimeout("", "request timeout", "")
		errMarshalled, _ := proto.Marshal(&err.Status)
		m := &message.Message{
			Type:  message.Response,
			ID:    pendingReq.msg.ID,
			Route: pendingReq.msg.Route,
			Data:  errMarshalled,
			Err:   true,
		}
		if !gen.deliver(m) {
			return false
		}
	}
	return true
}

func (c *Client) handlePackets(gen *connectionGeneration) {
	for {
		select {
		case p := <-gen.packetChan:
			if p == nil {
				continue
			}
			switch p.Type {
			case packet.Data:
				Log.Debug("client received data packet", zap.Int("payload_length", len(p.Data)))
				m, err := message.Decode(p.Data)
				if err != nil {
					Log.Error("error decoding message from server",
						zap.Uint8("packet_type", uint8(p.Type)),
						zap.Int("payload_length", len(p.Data)),
						zap.Error(err),
					)
					c.disconnectGeneration(gen, CloseReasonError)
					return
				}
				if m.Type == message.Response {
					pending, ok := gen.takePending(m.ID)
					if !ok {
						continue // do not process msg for already timedout request
					}
					if m.Route == "" {
						m.Route = pending.msg.Route
					}
				}
				if !gen.deliver(m) {
					return
				}
			case packet.Kick:
				Log.Warn("got kick packet from the server! disconnecting...")
				c.disconnectGeneration(gen, CloseReasonKicked)
				return
			}
		case <-gen.closeCh:
			return
		}
	}
}

func readPacket(reader io.Reader) (*packet.Packet, error) {
	header := make([]byte, codec.HeadLength)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, err
	}

	length, typ, err := codec.ParseHeader(header)
	if err != nil {
		return nil, err
	}
	p := &packet.Packet{Type: typ, Length: length, Data: make([]byte, length)}
	if _, err := io.ReadFull(reader, p.Data); err != nil {
		return nil, err
	}
	return p, nil
}

func (c *Client) handleServerMessages(gen *connectionGeneration) {
	defer c.disconnectGeneration(gen, CloseReasonError)
	for {
		p, err := readPacket(gen.conn)
		if err != nil {
			select {
			case <-gen.closeCh:
				return
			default:
				Log.Error("", zap.Error(err))
				return
			}
		}
		gen.recordServerActivity(time.Now())
		select {
		case gen.packetChan <- p:
		case <-gen.closeCh:
			return
		}
	}
}

func (c *Client) sendHeartbeats(gen *connectionGeneration, interval int) {
	t := time.NewTicker(time.Duration(interval) * time.Second)
	heartbeatTimeout := time.Duration(interval) * time.Second * 2
	defer func() {
		t.Stop()
		c.disconnectGeneration(gen, CloseReasonError)
	}()
	for {
		select {
		case <-t.C:
			deadline := time.Now().Add(-heartbeatTimeout)
			lastAt := gen.lastServerActivity()
			if lastAt.Before(deadline) {
				logger.Zap.Debug("Session heartbeat timeout", zap.Time("LastTime", lastAt), zap.Time("Deadline", deadline))
				return
			}

			p, err := gen.packetEncoder.Encode(packet.Heartbeat, []byte{})
			if err == nil {
				_, err = gen.safeWrite(p, defaultClientIOTimeout)
			}
			if err != nil {
				Log.Error("error sending heartbeat to server", zap.Error(err))
				return
			}
		case <-gen.closeCh:
			return
		}
	}
}

func (gen *connectionGeneration) recordServerActivity(at time.Time) {
	gen.lastActivityTick.Store(at.Sub(clientActivityEpoch).Nanoseconds())
}

func (gen *connectionGeneration) lastServerActivity() time.Time {
	return clientActivityEpoch.Add(time.Duration(gen.lastActivityTick.Load()))
}

// Disconnect disconnects the client
func (c *Client) Disconnect(reason CloseReason) {
	c.connMutex.Lock()
	if c.state == clientStateConnecting && c.attempt != nil {
		attempt := c.attempt
		attempt.cancelledByDisconnect = true
		attempt.cancel()
		conn := attempt.conn
		c.connMutex.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		attempt.done.Wait()
		return
	}
	gen := c.current
	c.connMutex.Unlock()
	if gen != nil {
		c.disconnectGeneration(gen, reason)
	}
}

func (c *Client) disconnectGeneration(gen *connectionGeneration, reason CloseReason) {
	var callback func(reason CloseReason)
	gen.closeOnce.Do(func() {
		gen.pendingMu.Lock()
		gen.closed = true
		for id := range gen.pending {
			delete(gen.pending, id)
			<-gen.slots
		}
		gen.pendingMu.Unlock()

		close(gen.closeCh)
		_ = gen.conn.Close()

		c.connMutex.Lock()
		if c.current == gen {
			c.current = nil
			c.state = clientStateIdle
			c.Connected = false
			callback = c.onDisconnected
		}
		c.connMutex.Unlock()

	})
	if callback != nil {
		callback(reason)
	}
}

func (c *Client) OnDisconnected(callback func(reason CloseReason)) {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	c.onDisconnected = callback
}

func cloneTLSConfig(configs []*tls.Config) (*tls.Config, error) {
	if len(configs) == 0 {
		return &tls.Config{}, nil
	}
	if configs[0] == nil {
		return nil, errors.New("client: nil TLS config")
	}
	return configs[0].Clone(), nil
}

func (c *Client) dial(ctx context.Context, u *url.URL, tlsCfg *tls.Config) (net.Conn, error) {
	switch u.Scheme {
	case "wss":
		// Preserve the ak-35 TLS policy; only mutate this connection's clone.
		tlsCfg.InsecureSkipVerify = true
		dialer := *websocket.DefaultDialer
		dialer.TLSClientConfig = tlsCfg
		wsconn, _, err := dialer.DialContext(ctx, u.String(), nil)
		if err != nil {
			return nil, err
		}
		conn, err := acceptor.NewWSConn(wsconn)
		if err != nil {
			_ = wsconn.Close()
			return nil, err
		}
		return conn, nil
	case "ws":
		dialer := *websocket.DefaultDialer
		wsconn, _, err := dialer.DialContext(ctx, u.String(), nil)
		if err != nil {
			return nil, err
		}
		conn, err := acceptor.NewWSConn(wsconn)
		if err != nil {
			_ = wsconn.Close()
			return nil, err
		}
		return conn, nil
	case "tcps", "tls":
		// Preserve the ak-35 TLS policy; only mutate this connection's clone.
		tlsCfg.InsecureSkipVerify = true
		rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(rawConn, tlsCfg)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
		return tlsConn, nil
	case "tcp":
		return (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	default:
		return nil, errors.New("unSupport schme:" + u.Scheme)
	}
}

// ConnectTo connects to the server at addr, for now the only supported protocol is tcp
// if tlsConfig is sent, it connects using TLS
func (c *Client) ConnectTo(uri string, tlsConfig ...*tls.Config) error {
	_, err := c.connectToContext(context.Background(), uri, tlsConfig)
	return err
}

func (c *Client) connectToContext(
	parent context.Context,
	uri string,
	tlsConfig []*tls.Config,
) (*connectionGeneration, error) {
	if !strings.Contains(uri, "://") {
		uri = "tcp://" + uri
	}
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := cloneTLSConfig(tlsConfig)
	if err != nil {
		return nil, err
	}
	if parent == nil {
		parent = context.Background()
	}

	c.connMutex.Lock()
	switch c.state {
	case clientStateConnected:
		c.connMutex.Unlock()
		return nil, errClientAlreadyConnected
	case clientStateConnecting:
		c.connMutex.Unlock()
		return nil, errClientConnecting
	}
	ctx, cancel := context.WithTimeout(parent, c.handshakeTimeout)
	attempt := &connectAttempt{
		ctx:            ctx,
		cancel:         cancel,
		handshakeData:  c.clientHandshakeData,
		packetEncoder:  c.packetEncoder,
		messageEncoder: c.messageEncoder,
	}
	attempt.done.Add(1)
	c.attempt = attempt
	c.state = clientStateConnecting
	c.connMutex.Unlock()

	defer func() {
		attempt.cancel()
		attempt.done.Done()
	}()

	connection, err := c.dial(attempt.ctx, u, tlsCfg)
	if err != nil {
		c.failConnectAttempt(attempt, nil)
		return nil, c.connectAttemptError(attempt, err)
	}

	c.connMutex.Lock()
	if c.attempt != attempt || c.state != clientStateConnecting {
		c.connMutex.Unlock()
		_ = connection.Close()
		return nil, errClientConnectionClosed
	}
	attempt.conn = connection
	c.connMutex.Unlock()

	cancelComplete := make(chan struct{})
	stopCancel := context.AfterFunc(attempt.ctx, func() {
		_ = connection.Close()
		close(cancelComplete)
	})

	heartbeat, err := c.handleHandshake(attempt, connection)
	if !stopCancel() {
		<-cancelComplete
	}
	if err == nil && attempt.ctx.Err() != nil {
		err = attempt.ctx.Err()
	}
	if err != nil {
		c.failConnectAttempt(attempt, connection)
		return nil, c.connectAttemptError(attempt, err)
	}

	c.connMutex.Lock()
	if c.attempt != attempt || c.state != clientStateConnecting || attempt.ctx.Err() != nil {
		c.connMutex.Unlock()
		c.failConnectAttempt(attempt, connection)
		return nil, c.connectAttemptError(attempt, errClientConnectionClosed)
	}
	gen := newConnectionGeneration(
		connection,
		attempt.packetEncoder,
		attempt.messageEncoder,
	)
	c.current = gen
	c.IncomingMsgChan = gen.incomingChan
	c.Connected = true
	c.state = clientStateConnected
	c.attempt = nil
	c.startGenerationLoops(gen, heartbeat)
	c.connMutex.Unlock()

	return gen, nil
}

func (c *Client) failConnectAttempt(attempt *connectAttempt, conn net.Conn) {
	if conn != nil {
		_ = conn.Close()
	}
	c.connMutex.Lock()
	if c.attempt == attempt {
		c.attempt = nil
		c.state = clientStateIdle
		c.Connected = false
	}
	c.connMutex.Unlock()
}

func (c *Client) connectAttemptError(attempt *connectAttempt, fallback error) error {
	c.connMutex.Lock()
	cancelledByDisconnect := attempt.cancelledByDisconnect
	c.connMutex.Unlock()
	if cancelledByDisconnect {
		return errClientConnectionClosed
	}
	if err := attempt.ctx.Err(); err != nil {
		return err
	}
	if netErr, ok := fallback.(net.Error); ok && netErr.Timeout() {
		if deadline, hasDeadline := attempt.ctx.Deadline(); hasDeadline && !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
	}
	return fallback
}

func (c *Client) handleHandshake(attempt *connectAttempt, conn net.Conn) (int, error) {
	deadline := time.Now().Add(c.handshakeTimeout)
	if contextDeadline, ok := attempt.ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return 0, fmt.Errorf("set handshake read deadline: %w", err)
	}
	if err := c.sendHandshakeRequestTo(conn, attempt.handshakeData, attempt.packetEncoder); err != nil {
		return 0, err
	}
	heartbeat, err := c.receiveHandshakeResponseFrom(conn, attempt.packetEncoder)
	if err != nil {
		return 0, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return 0, fmt.Errorf("clear handshake read deadline: %w", err)
	}
	return heartbeat, nil
}

// SendRequest sends a request to the server
func (c *Client) SendRequest(route string, data []byte) (uint, error) {
	return c.sendMsg(message.Request, route, data)
}

// SendNotify sends a notify to the server
func (c *Client) SendNotify(route string, data []byte) error {
	_, err := c.sendMsg(message.Notify, route, data)
	return err
}

func buildPacket(messageEncoder message.Encoder, packetEncoder codec.PacketEncoder, msg message.Message) ([]byte, error) {
	encMsg, err := messageEncoder.Encode(&msg)
	if err != nil {
		return nil, err
	}
	p, err := packetEncoder.Encode(packet.Data, encMsg)
	if err != nil {
		return nil, err
	}

	return p, nil
}

func (c *Client) currentGeneration() *connectionGeneration {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	return c.current
}

func (gen *connectionGeneration) registerPending(pending *pendingRequest) error {
	select {
	case gen.slots <- struct{}{}:
	case <-gen.closeCh:
		return errClientConnectionClosed
	}

	gen.pendingMu.Lock()
	defer gen.pendingMu.Unlock()
	if gen.closed {
		<-gen.slots
		return errClientConnectionClosed
	}
	if _, exists := gen.pending[pending.msg.ID]; exists {
		<-gen.slots
		return fmt.Errorf("client: duplicate request id %d", pending.msg.ID)
	}
	gen.pending[pending.msg.ID] = pending
	return nil
}

// sendMsg sends the request to the server
func (c *Client) sendMsg(msgType message.Type, route string, data []byte) (uint, error) {
	return c.sendMsgTo(c.currentGeneration(), msgType, route, data)
}

func (c *Client) sendMsgTo(gen *connectionGeneration, msgType message.Type, route string, data []byte) (uint, error) {
	if gen == nil {
		return 0, errClientConnectionClosed
	}
	m := message.Message{
		Type:  msgType,
		ID:    uint(atomic.AddUint32(&c.nextID, 1)),
		Route: route,
		Data:  data,
		Err:   false,
	}
	p, err := buildPacket(gen.messageEncoder, gen.packetEncoder, m)
	if err != nil {
		return m.ID, err
	}
	if msgType == message.Request {
		if err := gen.registerPending(&pendingRequest{msg: &m}); err != nil {
			return m.ID, err
		}
	}

	// Register before writing so an immediate response can find its request, but
	// start the timeout only after both admission and writer queues are passed.
	gen.writeMu.Lock()
	select {
	case <-gen.closeCh:
		err = errClientConnectionClosed
	default:
		gen.pendingMu.Lock()
		if pending := gen.pending[m.ID]; pending != nil {
			pending.sentAt = time.Now()
		}
		gen.pendingMu.Unlock()
		_, err = writeConnWithTimeout(gen.conn, p, defaultClientIOTimeout)
	}
	gen.writeMu.Unlock()
	if err != nil && msgType == message.Request {
		gen.takePending(m.ID)
	}
	return m.ID, err
}

func (c *Client) SafeWrite(b []byte) (n int, err error) {
	gen := c.currentGeneration()
	if gen == nil {
		return 0, errClientConnectionClosed
	}
	return gen.safeWrite(b, defaultClientIOTimeout)
}
