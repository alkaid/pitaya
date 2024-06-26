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
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/topfreegames/pitaya/v2/logger"

	"github.com/alkaid/goerrors/apierrors"
	"go.uber.org/zap"

	"github.com/topfreegames/pitaya/v2/acceptor"

	"github.com/gorilla/websocket"
	"github.com/topfreegames/pitaya/v2/conn/codec"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/conn/packet"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/util/compression"
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

// Client struct
type Client struct {
	conn                net.Conn
	Connected           bool
	packetEncoder       codec.PacketEncoder
	packetDecoder       codec.PacketDecoder
	packetChan          chan *packet.Packet
	IncomingMsgChan     chan *message.Message
	pendingChan         chan bool
	pendingRequests     map[uint]*pendingRequest
	pendingReqMutex     sync.Mutex
	requestTimeout      time.Duration
	closeChan           chan struct{}
	nextID              uint32
	messageEncoder      message.Encoder
	clientHandshakeData *session.HandshakeData
	onDisconnected      func(reason CloseReason)
	writeMutex          sync.Mutex
	lastAt              time.Time
	connMutex           sync.Mutex
}

// MsgChannel return the incoming message channel
func (c *Client) MsgChannel() chan *message.Message {
	return c.IncomingMsgChan
}

// ConnectedStatus return the connection status
func (c *Client) ConnectedStatus() bool {
	return c.Connected
}

// New returns a new client
func New(requestTimeout ...time.Duration) *Client {
	reqTimeout := 5 * time.Second
	if len(requestTimeout) > 0 {
		reqTimeout = requestTimeout[0]
	}

	return &Client{
		Connected:       false,
		packetEncoder:   codec.NewPomeloPacketEncoder(),
		packetDecoder:   codec.NewPomeloPacketDecoder(),
		packetChan:      make(chan *packet.Packet, 10),
		pendingRequests: make(map[uint]*pendingRequest),
		requestTimeout:  reqTimeout,
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

	_, err = c.connWriteWithDeadline(p)
	return err
}

func (c *Client) handleHandshakeResponse() error {
	c.lastAt = time.Now()
	buf := bytes.NewBuffer(nil)
	packets, err := c.readPackets(buf)
	if err != nil {
		return err
	}

	handshakePacket := packets[0]
	if handshakePacket.Type != packet.Handshake {
		return fmt.Errorf("got first packet from server that is not a handshake, aborting")
	}

	handshake := &HandshakeData{}
	if compression.IsCompressed(handshakePacket.Data) {
		handshakePacket.Data, err = compression.InflateData(handshakePacket.Data)
		if err != nil {
			return err
		}
	}

	err = json.Unmarshal(handshakePacket.Data, handshake)
	if err != nil {
		return err
	}

	Log.Debug("got handshake from sv", zap.Any("data", handshake))

	if handshake.Sys.Dict != nil {
		message.SetDictionary(handshake.Sys.Dict)
	}
	p, err := c.packetEncoder.Encode(packet.HandshakeAck, []byte{})
	if err != nil {
		return err
	}
	_, err = c.connWriteWithDeadline(p)
	if err != nil {
		return err
	}

	c.Connected = true

	go c.sendHeartbeats(handshake.Sys.Heartbeat)
	go c.handleServerMessages()
	go c.handlePackets()
	go c.pendingRequestsReaper()

	return nil
}
func (c *Client) connWriteWithDeadline(b []byte) (int, error) {
	err := c.conn.SetWriteDeadline(time.Now().Add(time.Second * 5))
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
			switch p.Type {
			case packet.Data:
				// handle data
				Log.Debug("client handle packets got", zap.String("data", string(p.Data)))
				m, err := message.Decode(p.Data)
				if err != nil {
					Log.Error("error decoding msg from sv", zap.String("data", string(m.Data)))
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
			}
		case <-c.closeChan:
			return
		}
	}
}

func (c *Client) readPackets(buf *bytes.Buffer) ([]*packet.Packet, error) {
	// listen for sv messages
	data := make([]byte, 1024)
	n := len(data)
	var err error

	for n == len(data) {
		n, err = c.conn.Read(data)
		if err != nil {
			return nil, err
		}
		buf.Write(data[:n])
	}
	packets, err := c.packetDecoder.Decode(buf.Bytes())
	if err != nil {
		Log.Error("error decoding packet from server", zap.Error(err))
	}
	totalProcessed := 0
	for _, p := range packets {
		totalProcessed += codec.HeadLength + p.Length
	}
	buf.Next(totalProcessed)

	return packets, nil
}

func (c *Client) handleServerMessages() {
	buf := bytes.NewBuffer(nil)
	defer c.Disconnect(CloseReasonError)
	for c.Connected {
		packets, err := c.readPackets(buf)
		if err != nil && c.Connected {
			Log.Error("", zap.Error(err))
			break
		}
		c.lastAt = time.Now()

		for _, p := range packets {
			c.packetChan <- p
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
			if c.lastAt.Before(deadline) {
				logger.Zap.Debug("Session heartbeat timeout", zap.Time("LastTime", c.lastAt), zap.Time("Deadline", deadline))
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

// Disconnect disconnects the client
func (c *Client) Disconnect(reason CloseReason) {
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	if c.Connected {
		c.Connected = false
		close(c.closeChan)
		c.conn.Close()
		if c.onDisconnected != nil {
			c.onDisconnected(reason)
		}
	}
}

func (c *Client) OnDisconnected(callback func(reason CloseReason)) {
	c.onDisconnected = callback
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
	var tlsCfg *tls.Config
	if len(tlsConfig) > 0 {
		tlsCfg = tlsConfig[0]
	} else {
		tlsCfg = &tls.Config{}
	}
	c.connMutex.Lock()
	defer c.connMutex.Unlock()
	switch u.Scheme {
	case "wss":
		tlsCfg.InsecureSkipVerify = true
		fallthrough
	case "ws":
		dialer := websocket.DefaultDialer
		dialer.TLSClientConfig = tlsCfg
		var wsconn *websocket.Conn
		wsconn, _, err = dialer.Dial(u.String(), nil)
		if err != nil {
			return err
		}
		c.conn, err = acceptor.NewWSConn(wsconn)
		if err != nil {
			return err
		}
	case "tcps", "tls":
		tlsCfg.InsecureSkipVerify = true
		c.conn, err = tls.Dial("tcp", u.Host, tlsCfg)
	case "tcp":
		c.conn, err = net.Dial("tcp", u.Host)
	default:
		return errors.New("unSupport schme:" + u.Scheme)
	}
	if err != nil {
		return err
	}
	c.IncomingMsgChan = make(chan *message.Message, 10)

	if err = c.handleHandshake(); err != nil {
		return err
	}

	c.closeChan = make(chan struct{})

	return nil
}

func (c *Client) handleHandshake() error {
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
