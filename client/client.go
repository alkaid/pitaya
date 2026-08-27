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

const defaultClientIOTimeout = 5 * time.Second

// Client struct
type Client struct {
	conn                net.Conn
	Connected           bool
	packetEncoder       codec.PacketEncoder
	packetChan          chan *packet.Packet
	IncomingMsgChan     chan *message.Message
	pendingChan         chan bool
	pendingRequests     map[uint]*pendingRequest
	pendingReqMutex     sync.Mutex
	requestTimeout      time.Duration
	handshakeTimeout    time.Duration
	closeChan           chan struct{}
	nextID              uint32
	messageEncoder      message.Encoder
	clientHandshakeData *session.HandshakeData
	onDisconnected      func(reason CloseReason)
	writeMutex          sync.Mutex
	lastActivityTick    atomic.Int64
	connMutex           sync.Mutex
}

// MsgChannel return the incoming message channel
func (c *Client) MsgChannel() chan *message.Message {
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
		packetEncoder:    codec.NewPomeloPacketEncoder(),
		packetChan:       make(chan *packet.Packet, 10),
		pendingRequests:  make(map[uint]*pendingRequest),
		requestTimeout:   reqTimeout,
		handshakeTimeout: defaultClientIOTimeout,
		// 30 here is the limit of inflight messages
		// TODO this should probably be configurable
		pendingChan:    make(chan bool, 30),
		messageEncoder: message.NewMessagesEncoder(false),
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
	c.clientHandshakeData = data
}

func (c *Client) sendHandshakeRequest() error {
	enc, err := json.Marshal(c.clientHandshakeData)
	if err != nil {
		return err
	}

	p, err := c.packetEncoder.Encode(packet.Handshake, enc)
	if err != nil {
		return err
	}

	_, err = c.connWriteWithTimeout(p, c.handshakeTimeout)
	return err
}

func (c *Client) handleHandshakeResponse() error {
	heartbeat, err := c.receiveHandshakeResponse()
	if err != nil {
		return err
	}
	if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear handshake read deadline: %w", err)
	}
	c.startConnectionLoops(heartbeat)
	return nil
}

func (c *Client) receiveHandshakeResponse() (int, error) {
	c.recordServerActivity(time.Now())
	handshakePacket, err := readPacket(c.conn)
	if err != nil {
		return 0, err
	}

	if handshakePacket.Type != packet.Handshake {
		return 0, fmt.Errorf("got first packet from server that is not a handshake, aborting")
	}

	handshake := &HandshakeData{}
	if compression.IsCompressed(handshakePacket.Data) {
		handshakePacket.Data, err = compression.InflateDataLimit(handshakePacket.Data, codec.MaxPacketSize)
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
	p, err := c.packetEncoder.Encode(packet.HandshakeAck, []byte{})
	if err != nil {
		return 0, err
	}
	_, err = c.connWriteWithTimeout(p, c.handshakeTimeout)
	if err != nil {
		return 0, err
	}
	return handshake.Sys.Heartbeat, nil
}

func (c *Client) startConnectionLoops(heartbeat int) {
	c.Connected = true

	if heartbeat > 0 {
		go c.sendHeartbeats(heartbeat)
	}
	go c.handleServerMessages()
	go c.handlePackets()
	go c.pendingRequestsReaper()
}

func (c *Client) connWriteWithDeadline(b []byte) (int, error) {
	return c.connWriteWithTimeout(b, defaultClientIOTimeout)
}

func (c *Client) connWriteWithTimeout(b []byte, timeout time.Duration) (int, error) {
	err := c.conn.SetWriteDeadline(time.Now().Add(timeout))
	if err != nil {
		return 0, err
	}
	n, err := c.conn.Write(b)
	if err != nil {
		return n, err
	}
	return n, nil
}

// pendingRequestsReaper delete timedout requests
func (c *Client) pendingRequestsReaper() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			toDelete := make([]*pendingRequest, 0)
			c.pendingReqMutex.Lock()
			for _, v := range c.pendingRequests {
				if time.Now().Sub(v.sentAt) > c.requestTimeout {
					toDelete = append(toDelete, v)
				}
			}
			for _, pendingReq := range toDelete {
				err := apierrors.GatewayTimeout("", "request timeout", "")
				errMarshalled, _ := proto.Marshal(&err.Status)
				// send a timeout to incoming msg chan
				m := &message.Message{
					Type:  message.Response,
					ID:    pendingReq.msg.ID,
					Route: pendingReq.msg.Route,
					Data:  errMarshalled,
					Err:   true,
				}
				delete(c.pendingRequests, pendingReq.msg.ID)
				<-c.pendingChan
				c.IncomingMsgChan <- m
			}
			c.pendingReqMutex.Unlock()
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) handlePackets() {
	for {
		select {
		case p := <-c.packetChan:
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
					c.Disconnect(CloseReasonError)
					return
				}
				if m.Type == message.Response {
					c.pendingReqMutex.Lock()
					if _, ok := c.pendingRequests[m.ID]; ok {
						delete(c.pendingRequests, m.ID)
						<-c.pendingChan
					} else {
						c.pendingReqMutex.Unlock()
						continue // do not process msg for already timedout request
					}
					c.pendingReqMutex.Unlock()
				}
				c.IncomingMsgChan <- m
			case packet.Kick:
				Log.Warn("got kick packet from the server! disconnecting...")
				c.Disconnect(CloseReasonKicked)
				return
			}
		case <-c.closeChan:
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

func (c *Client) handleServerMessages() {
	defer c.Disconnect(CloseReasonError)
	for {
		p, err := readPacket(c.conn)
		if err != nil {
			select {
			case <-c.closeChan:
				return
			default:
				Log.Error("", zap.Error(err))
				return
			}
		}
		c.recordServerActivity(time.Now())
		select {
		case c.packetChan <- p:
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) sendHeartbeats(interval int) {
	t := time.NewTicker(time.Duration(interval) * time.Second)
	heartbeatTimeout := time.Duration(interval) * time.Second * 2
	defer func() {
		t.Stop()
		c.Disconnect(CloseReasonError)
	}()
	for {
		select {
		case <-t.C:
			deadline := time.Now().Add(-heartbeatTimeout)
			lastAt := c.lastServerActivity()
			if lastAt.Before(deadline) {
				logger.Zap.Debug("Session heartbeat timeout", zap.Time("LastTime", lastAt), zap.Time("Deadline", deadline))
				return
			}

			p, _ := c.packetEncoder.Encode(packet.Heartbeat, []byte{})
			_, err := c.SafeWrite(p)
			if err != nil {
				Log.Error("error sending heartbeat to server", zap.Error(err))
				return
			}
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) recordServerActivity(at time.Time) {
	c.lastActivityTick.Store(at.Sub(clientActivityEpoch).Nanoseconds())
}

func (c *Client) lastServerActivity() time.Time {
	return clientActivityEpoch.Add(time.Duration(c.lastActivityTick.Load()))
}

// Disconnect disconnects the client
func (c *Client) Disconnect(reason CloseReason) {
	c.connMutex.Lock()
	if !c.Connected {
		c.connMutex.Unlock()
		return
	}
	c.Connected = false
	close(c.closeChan)
	_ = c.conn.Close()
	callback := c.onDisconnected
	c.connMutex.Unlock()

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

func configureTLSServerName(u *url.URL, tlsCfg *tls.Config) error {
	if tlsCfg.ServerName != "" {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("client: TLS target %q has no host", u.Host)
	}
	tlsCfg.ServerName = host
	return nil
}

func (c *Client) dial(u *url.URL, tlsCfg *tls.Config) (net.Conn, error) {
	switch u.Scheme {
	case "wss":
		if err := configureTLSServerName(u, tlsCfg); err != nil {
			return nil, err
		}
		dialer := *websocket.DefaultDialer
		dialer.TLSClientConfig = tlsCfg
		ctx, cancel := context.WithTimeout(context.Background(), c.handshakeTimeout)
		defer cancel()
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
		ctx, cancel := context.WithTimeout(context.Background(), c.handshakeTimeout)
		defer cancel()
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
		if err := configureTLSServerName(u, tlsCfg); err != nil {
			return nil, err
		}
		rawConn, err := net.DialTimeout("tcp", u.Host, c.handshakeTimeout)
		if err != nil {
			return nil, err
		}
		tlsConn := tls.Client(rawConn, tlsCfg)
		ctx, cancel := context.WithTimeout(context.Background(), c.handshakeTimeout)
		defer cancel()
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
		return tlsConn, nil
	case "tcp":
		return net.DialTimeout("tcp", u.Host, c.handshakeTimeout)
	default:
		return nil, errors.New("unSupport schme:" + u.Scheme)
	}
}

// ConnectTo connects to the server at addr, for now the only supported protocol is tcp
// if tlsConfig is sent, it connects using TLS
func (c *Client) ConnectTo(uri string, tlsConfig ...*tls.Config) error {
	if !strings.Contains(uri, "://") {
		uri = "tcp://" + uri
	}
	u, err := url.ParseRequestURI(uri)
	if err != nil {
		return err
	}
	tlsCfg, err := cloneTLSConfig(tlsConfig)
	if err != nil {
		return err
	}
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	if c.Connected {
		return errors.New("client: already connected")
	}
	c.conn, err = c.dial(u, tlsCfg)
	if err != nil {
		return err
	}
	connected := false
	defer func() {
		if !connected {
			_ = c.conn.Close()
			c.conn = nil
		}
	}()
	c.IncomingMsgChan = make(chan *message.Message, 10)
	c.closeChan = make(chan struct{})
	c.packetChan = make(chan *packet.Packet, 10)

	if err = c.handleHandshake(); err != nil {
		return err
	}

	connected = true
	return nil
}

func (c *Client) handleHandshake() error {
	if err := c.conn.SetReadDeadline(time.Now().Add(c.handshakeTimeout)); err != nil {
		return fmt.Errorf("set handshake read deadline: %w", err)
	}
	if err := c.sendHandshakeRequest(); err != nil {
		return err
	}

	if err := c.handleHandshakeResponse(); err != nil {
		return err
	}
	return nil
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

func (c *Client) buildPacket(msg message.Message) ([]byte, error) {
	encMsg, err := c.messageEncoder.Encode(&msg)
	if err != nil {
		return nil, err
	}
	p, err := c.packetEncoder.Encode(packet.Data, encMsg)
	if err != nil {
		return nil, err
	}

	return p, nil
}

// sendMsg sends the request to the server
func (c *Client) sendMsg(msgType message.Type, route string, data []byte) (uint, error) {
	// TODO mount msg and encode
	m := message.Message{
		Type:  msgType,
		ID:    uint(atomic.AddUint32(&c.nextID, 1)),
		Route: route,
		Data:  data,
		Err:   false,
	}
	p, err := c.buildPacket(m)
	if msgType == message.Request {
		c.pendingChan <- true
		c.pendingReqMutex.Lock()
		if _, ok := c.pendingRequests[m.ID]; !ok {
			c.pendingRequests[m.ID] = &pendingRequest{
				msg:    &m,
				sentAt: time.Now(),
			}
		}
		c.pendingReqMutex.Unlock()
	}

	if err != nil {
		return m.ID, err
	}
	_, err = c.SafeWrite(p)
	return m.ID, err
}

func (c *Client) SafeWrite(b []byte) (n int, err error) {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()
	return c.connWriteWithDeadline(b)
}
