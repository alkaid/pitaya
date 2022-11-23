// Copyright (c) nano Author and TFG Co. All Rights Reserved.
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

package agent

import (
	"context"
	"encoding/binary"
	gojson "encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/topfreegames/pitaya/v2/acceptor"
	"go.uber.org/zap"

	"go.opentelemetry.io/otel/trace"

	"github.com/alkaid/goerrors/apierrors"

	"github.com/topfreegames/pitaya/v2/co"
	"github.com/topfreegames/pitaya/v2/conn/codec"
	"github.com/topfreegames/pitaya/v2/conn/message"
	"github.com/topfreegames/pitaya/v2/conn/packet"
	"github.com/topfreegames/pitaya/v2/constants"
	"github.com/topfreegames/pitaya/v2/logger"
	"github.com/topfreegames/pitaya/v2/metrics"
	"github.com/topfreegames/pitaya/v2/protos"
	"github.com/topfreegames/pitaya/v2/serialize"
	"github.com/topfreegames/pitaya/v2/session"
	"github.com/topfreegames/pitaya/v2/tracing"
	"github.com/topfreegames/pitaya/v2/util"
	"github.com/topfreegames/pitaya/v2/util/compression"
)

var (
	// hbd contains the heartbeat packet data
	hbd []byte
	// hbd contains the heartbeat ack packet data
	hbAck []byte
	// hrd contains the handshake response data
	hrd  []byte
	once sync.Once
)

const handlerType = "handler"

type (
	agentImpl struct {
		Session            session.Session // session
		sessionPool        session.SessionPool
		appDieChan         chan bool         // app die channel
		chDie              chan struct{}     // wait for close
		chSend             chan pendingWrite // push message queue
		chStopHeartbeat    chan struct{}     // stop heartbeats
		chStopWrite        chan struct{}     // stop writing messages
		closeMutex         sync.Mutex
		conn               net.Conn            // low-level conn fd
		decoder            codec.PacketDecoder // binary decoder
		encoder            codec.PacketEncoder // binary encoder
		heartbeatTimeout   time.Duration
		lastAt             int64 // last heartbeat unix time stamp
		messageEncoder     message.Encoder
		messagesBufferSize int // size of the pending messages buffer
		metricsReporters   []metrics.Reporter
		serializer         serialize.Serializer // message serializer
		state              int32                // current agent state
	}

	pendingMessage struct {
		ctx     context.Context
		typ     message.Type // message type
		route   string       // message route (push)
		mid     uint         // response message id (response)
		payload interface{}  // payload
		err     bool         // if its an error message
	}

	pendingWrite struct {
		ctx  context.Context
		data []byte
		err  error
	}

	// Agent corresponds to a user and is used for storing raw Conn information
	Agent interface {
		GetSession() session.Session
		Push(route string, v interface{}) error
		ResponseMID(ctx context.Context, mid uint, v interface{}, isError ...bool) error
		Close(callback map[string]string, reason ...session.CloseReason) error
		RemoteAddr() net.Addr
		String() string
		GetStatus() int32
		Kick(ctx context.Context, reason ...session.CloseReason) error
		SetLastAt()
		SetStatus(state int32)
		Handle()
		IPVersion() string
		SendHandshakeResponse() error
		SendHeartbeatResponse(unixMillTime int64) error
		SendRequest(ctx context.Context, serverID, route string, v interface{}) (*protos.Response, error)
		AnswerWithError(ctx context.Context, mid uint, err error)
	}

	// AgentFactory factory for creating Agent instances
	AgentFactory interface {
		CreateAgent(conn net.Conn) Agent
	}

	agentFactoryImpl struct {
		sessionPool        session.SessionPool
		appDieChan         chan bool           // app die channel
		decoder            codec.PacketDecoder // binary decoder
		encoder            codec.PacketEncoder // binary encoder
		heartbeatTimeout   time.Duration
		messageEncoder     message.Encoder
		messagesBufferSize int // size of the pending messages buffer
		metricsReporters   []metrics.Reporter
		serializer         serialize.Serializer // message serializer
	}
)

// NewAgentFactory ctor
func NewAgentFactory(
	appDieChan chan bool,
	decoder codec.PacketDecoder,
	encoder codec.PacketEncoder,
	serializer serialize.Serializer,
	heartbeatTimeout time.Duration,
	messageEncoder message.Encoder,
	messagesBufferSize int,
	sessionPool session.SessionPool,
	metricsReporters []metrics.Reporter,
) AgentFactory {
	return &agentFactoryImpl{
		appDieChan:         appDieChan,
		decoder:            decoder,
		encoder:            encoder,
		heartbeatTimeout:   heartbeatTimeout,
		messageEncoder:     messageEncoder,
		messagesBufferSize: messagesBufferSize,
		sessionPool:        sessionPool,
		metricsReporters:   metricsReporters,
		serializer:         serializer,
	}
}

// CreateAgent returns a new agent
func (f *agentFactoryImpl) CreateAgent(conn net.Conn) Agent {
	return newAgent(conn, f.decoder, f.encoder, f.serializer, f.heartbeatTimeout, f.messagesBufferSize, f.appDieChan, f.messageEncoder, f.metricsReporters, f.sessionPool)
}

// NewAgent create new agent instance
func newAgent(
	conn net.Conn,
	packetDecoder codec.PacketDecoder,
	packetEncoder codec.PacketEncoder,
	serializer serialize.Serializer,
	heartbeatTime time.Duration,
	messagesBufferSize int,
	dieChan chan bool,
	messageEncoder message.Encoder,
	metricsReporters []metrics.Reporter,
	sessionPool session.SessionPool,
) Agent {
	// initialize heartbeat and handshake data on first user connection
	serializerName := serializer.GetName()

	once.Do(func() {
		hbdEncode(heartbeatTime, packetEncoder, messageEncoder.IsCompressionEnabled(), serializerName)
	})

	a := &agentImpl{
		appDieChan:         dieChan,
		chDie:              make(chan struct{}),
		chSend:             make(chan pendingWrite, messagesBufferSize),
		chStopHeartbeat:    make(chan struct{}),
		chStopWrite:        make(chan struct{}),
		messagesBufferSize: messagesBufferSize,
		conn:               conn,
		decoder:            packetDecoder,
		encoder:            packetEncoder,
		heartbeatTimeout:   heartbeatTime,
		lastAt:             time.Now().Unix(),
		serializer:         serializer,
		state:              constants.StatusStart,
		messageEncoder:     messageEncoder,
		metricsReporters:   metricsReporters,
		sessionPool:        sessionPool,
	}

	// binding session
	s := sessionPool.NewSession(a, true)
	metrics.ReportNumberOfConnectedClients(metricsReporters, sessionPool.GetSessionCount())
	a.Session = s
	return a
}

func (a *agentImpl) getMessageFromPendingMessage(pm pendingMessage) (*message.Message, error) {
	payload, err := util.SerializeOrRaw(a.serializer, pm.payload)
	if err != nil {
		payload, err = util.GetErrorPayload(a.serializer, err)
		if err != nil {
			return nil, err
		}
	}

	// construct message and encode
	m := &message.Message{
		Type:  pm.typ,
		Data:  payload,
		Route: pm.route,
		ID:    pm.mid,
		Err:   pm.err,
	}

	return m, nil
}

func (a *agentImpl) packetEncodeMessage(m *message.Message) ([]byte, error) {
	em, err := a.messageEncoder.Encode(m)
	if err != nil {
		return nil, err
	}

	// packet encode
	p, err := a.encoder.Encode(packet.Data, em)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (a *agentImpl) send(pendingMsg pendingMessage) (err error) {
	defer func() {
		if e := recover(); e != nil {
			err = apierrors.ClientClosed("pitaya.BrokenPipe", constants.ErrBrokenPipe.Error(), "")
		}
	}()
	a.reportChannelSize()

	m, err := a.getMessageFromPendingMessage(pendingMsg)
	if err != nil {
		return err
	}

	// packet encode
	p, err := a.packetEncodeMessage(m)
	if err != nil {
		return err
	}

	pWrite := pendingWrite{
		ctx:  pendingMsg.ctx,
		data: p,
	}

	if pendingMsg.err {
		pWrite.err = util.GetErrorFromPayload(a.serializer, m.Data)
	}

	// chSend is never closed so we need this to don't block if agent is already closed
	select {
	case a.chSend <- pWrite:
	case <-a.chDie:
	}
	return
}

// GetSession returns the agent session
func (a *agentImpl) GetSession() session.Session {
	return a.Session
}

// Push implementation for NetworkEntity interface
func (a *agentImpl) Push(route string, v interface{}) error {
	if a.GetStatus() == constants.StatusClosed {
		return apierrors.ClientClosed("ErrBrokenPipe", "agent push error", "").WithCause(constants.ErrBrokenPipe)
	}

	switch d := v.(type) {
	case []byte:
		logger.Zap.Debug("Type=Push", zap.Int64("ID", a.Session.ID()), zap.String("UID", a.Session.UID()), zap.String("Route", route), zap.Int("DataLen", len(d)))
	default:
		logger.Zap.Debug("Type=Push", zap.Int64("ID", a.Session.ID()), zap.String("UID", a.Session.UID()), zap.String("Route", route), zap.Any("Data", d))
	}
	return a.send(pendingMessage{typ: message.Push, route: route, payload: v})
}

// ResponseMID implementation for NetworkEntity interface
// Respond message to session
func (a *agentImpl) ResponseMID(ctx context.Context, mid uint, v interface{}, isError ...bool) error {
	err := false
	if len(isError) > 0 {
		err = isError[0]
	}
	if a.GetStatus() == constants.StatusClosed {
		return apierrors.ClientClosed("ErrBrokenPipe", constants.ErrBrokenPipe.Error(), "").WithCause(constants.ErrBrokenPipe)
	}

	if mid <= 0 {
		// return constants.ErrSessionOnNotify
		// 改成用push通知客户端系统错误
		if !err {
			return constants.ErrSessionOnNotify
		} else {
			return a.send(pendingMessage{
				ctx:     ctx,
				typ:     message.Push,
				route:   constants.ServerInternalErrorToClientRoute,
				mid:     0,
				payload: v,
				err:     err,
			})
		}
	}

	switch d := v.(type) {
	case []byte:
		logger.Zap.Debug("Type=Response", zap.Int64("ID", a.Session.ID()), zap.String("UID", a.Session.UID()), zap.Uint("MID", mid), zap.Int("DataLen", len(d)))
	default:
		logger.Zap.Info("Type=Response", zap.Int64("ID", a.Session.ID()), zap.String("UID", a.Session.UID()), zap.Uint("MID", mid), zap.Any("Data", d))
	}

	return a.send(pendingMessage{ctx: ctx, typ: message.Response, mid: mid, payload: v, err: err})
}

// Close closes the agent, cleans inner state and closes low-level connection.
// Any blocked Read or Write operations will be unblocked and return errors.
func (a *agentImpl) Close(callback map[string]string, reason ...session.CloseReason) error {
	a.closeMutex.Lock()
	defer a.closeMutex.Unlock()
	if a.GetStatus() == constants.StatusClosed {
		return constants.ErrCloseClosedSession
	}
	a.SetStatus(constants.StatusClosed)

	logger.Zap.Debug("Session closed",
		zap.Int64("ID", a.Session.ID()), zap.String("UID", a.Session.UID()), zap.Stringer("IP", a.conn.RemoteAddr()))

	// prevent closing closed channel
	select {
	case <-a.chDie:
		// expect
	default:
		close(a.chStopWrite)
		close(a.chStopHeartbeat)
		close(a.chDie)
		a.onSessionClosed(a.Session, callback, reason...)
	}

	metrics.ReportNumberOfConnectedClients(a.metricsReporters, a.sessionPool.GetSessionCount())
	closeReason := session.CloseReasonNormal
	if len(reason) > 0 {
		closeReason = reason[0]
	}
	// 若是websocket 且是被踢的 返回自定义reason
	if wsConn, ok := a.conn.(*acceptor.WSConn); ok && closeReason > session.CloseReasonKickMin {
		err := wsConn.InnerConn().WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, strconv.Itoa(closeReason)), time.Time{})
		logger.Zap.Warn("write close control message by kick reason error", zap.Error(err))
	}
	return a.conn.Close()
}

// RemoteAddr implementation for NetworkEntity interface
// returns the remote network address.
func (a *agentImpl) RemoteAddr() net.Addr {
	return a.conn.RemoteAddr()
}

// String, implementation for Stringer interface
func (a *agentImpl) String() string {
	return fmt.Sprintf("Remote=%s, LastTime=%d", a.conn.RemoteAddr().String(), atomic.LoadInt64(&a.lastAt))
}

// GetStatus gets the status
func (a *agentImpl) GetStatus() int32 {
	return atomic.LoadInt32(&a.state)
}

// Kick sends a kick packet to a client
func (a *agentImpl) Kick(ctx context.Context, reason ...session.CloseReason) error {
	// packet encode
	kickReason := session.CloseReasonKickRebind
	if len(reason) > 0 {
		kickReason = reason[0]
	}
	bs := make([]byte, 4)
	binary.BigEndian.PutUint32(bs, uint32(kickReason))
	p, err := a.encoder.Encode(packet.Kick, bs)
	if err != nil {
		return err
	}
	_, err = a.conn.Write(p)
	return err
}

// SetLastAt sets the last at to now
func (a *agentImpl) SetLastAt() {
	atomic.StoreInt64(&a.lastAt, time.Now().Unix())
}

// SetStatus sets the agent status
func (a *agentImpl) SetStatus(state int32) {
	atomic.StoreInt32(&a.state, state)
}

// Handle handles the messages from and to a client
func (a *agentImpl) Handle() {
	defer func() {
		a.Close(nil)
		logger.Zap.Debug("Session handle goroutine exit", zap.Int64("ID", a.Session.ID()), zap.String("UID", a.Session.UID()))
	}()

	co.Go(func() { a.write() })
	co.Go(func() { a.heartbeat() })
	<-a.chDie // agent closed signal
}

// IPVersion returns the remote address ip version.
// net.TCPAddr and net.UDPAddr implementations of String()
// always construct result as <ip>:<port> on both
// ipv4 and ipv6. Also, to see if the ip is ipv6 they both
// check if there is a colon on the string.
// So checking if there are more than one colon here is safe.
func (a *agentImpl) IPVersion() string {
	version := constants.IPv4

	ipPort := a.RemoteAddr().String()
	if strings.Count(ipPort, ":") > 1 {
		version = constants.IPv6
	}

	return version
}

func (a *agentImpl) heartbeat() {
	ticker := time.NewTicker(a.heartbeatTimeout)

	defer func() {
		ticker.Stop()
		a.Close(nil)
	}()

	for {
		select {
		case <-ticker.C:
			deadline := time.Now().Add(-2 * a.heartbeatTimeout).Unix()
			if atomic.LoadInt64(&a.lastAt) < deadline {
				logger.Zap.Debug("Session heartbeat timeout", zap.Int64("LastTime", atomic.LoadInt64(&a.lastAt)), zap.Int64("Deadline", deadline))
				return
			}
			bs := make([]byte, 8)
			binary.BigEndian.PutUint64(bs, uint64(time.Now().UnixMilli()))
			hbData, err := a.encoder.Encode(packet.Heartbeat, bs)
			if err != nil {
				logger.Zap.Error("encode hearbeat ")
			}
			// chSend is never closed so we need this to don't block if agent is already closed
			select {
			case a.chSend <- pendingWrite{data: hbData}:
			case <-a.chDie:
				return
			case <-a.chStopHeartbeat:
				return
			}
		case <-a.chDie:
			return
		case <-a.chStopHeartbeat:
			return
		}
	}
}

func (a *agentImpl) onSessionClosed(s session.Session, callback map[string]string, reason ...session.CloseReason) {
	defer func() {
		if err := recover(); err != nil {
			if err_, ok := err.(error); ok {
				logger.Zap.Error("pitaya/onSessionClosed", zap.Error(err_))
			} else {
				logger.Zap.Error("pitaya/onSessionClosed", zap.Any("recover", err))
			}
		}
	}()

	for _, fn1 := range s.GetOnCloseCallbacks() {
		fn1()
	}

	for _, fn2 := range a.sessionPool.GetSessionCloseCallbacks() {
		rea := session.CloseReasonNormal
		if len(reason) > 0 {
			rea = reason[0]
		}
		fn2(s, callback, rea)
	}
}

// SendHandshakeResponse sends a handshake response
func (a *agentImpl) SendHandshakeResponse() error {
	_, err := a.conn.Write(hrd)
	return err
}
func (a *agentImpl) SendHeartbeatResponse(unixMillTime int64) error {
	hbAckData := hbAck
	var err error
	if unixMillTime > 0 {
		bs := make([]byte, 8)
		binary.BigEndian.PutUint64(bs, uint64(unixMillTime))
		hbAckData, err = a.encoder.Encode(packet.HandshakeAck, bs)
		if err != nil {
			logger.Zap.Error("encode hearbeat ack error ")
			hbAckData = hbAck
		}
	}

	pWrite := pendingWrite{
		ctx:  context.Background(),
		data: hbAckData,
	}

	// chSend is never closed so we need this to don't block if agent is already closed
	select {
	case a.chSend <- pWrite:
	case <-a.chDie:
	}
	return nil
}

func (a *agentImpl) write() {
	// clean func
	defer func() {
		a.Close(nil)
	}()

	for {
		select {
		case pWrite := <-a.chSend:
			// close agent if low-level Conn broken
			if _, err := a.conn.Write(pWrite.data); err != nil {
				tracing.FinishSpan(pWrite.ctx, err)
				metrics.ReportTimingFromCtx(pWrite.ctx, a.metricsReporters, handlerType, err)
				logger.Log.Errorf("Failed to write in conn: %s", err.Error())
				return
			}
			var e error
			tracing.FinishSpan(pWrite.ctx, e)
			metrics.ReportTimingFromCtx(pWrite.ctx, a.metricsReporters, handlerType, pWrite.err)
		case <-a.chStopWrite:
			return
		}
	}
}

// SendRequest sends a request to a server
func (a *agentImpl) SendRequest(ctx context.Context, serverID, route string, v interface{}) (*protos.Response, error) {
	return nil, constants.ErrNotImplemented
}

// AnswerWithError answers with an error
func (a *agentImpl) AnswerWithError(ctx context.Context, mid uint, err error) {
	var e error
	defer func() {
		if e != nil {
			tracing.FinishSpan(ctx, e)
			metrics.ReportTimingFromCtx(ctx, a.metricsReporters, handlerType, e)
		}
	}()
	if ctx != nil && err != nil {
		s := trace.SpanFromContext(ctx)
		if s != nil {
			tracing.LogError(s, err)
		}
	}
	p, e := util.GetErrorPayload(a.serializer, err)
	if e != nil {
		logger.Zap.Error("error answering the user with an error", zap.Error(err))
		return
	}
	e = a.Session.ResponseMID(ctx, mid, p, true)
	if e != nil {
		logger.Zap.Error("error answering the user with an error", zap.Error(err))
	}
}

func hbdEncode(heartbeatTimeout time.Duration, packetEncoder codec.PacketEncoder, dataCompression bool, serializerName string) {
	hData := map[string]interface{}{
		"code": 200,
		"sys": map[string]interface{}{
			"heartbeat":  heartbeatTimeout.Seconds(),
			"dict":       message.GetDictionary(),
			"serializer": serializerName,
		},
	}
	data, err := gojson.Marshal(hData)
	if err != nil {
		panic(err)
	}

	if dataCompression {
		compressedData, err := compression.DeflateData(data)
		if err != nil {
			panic(err)
		}

		if len(compressedData) < len(data) {
			data = compressedData
		}
	}

	hrd, err = packetEncoder.Encode(packet.Handshake, data)
	if err != nil {
		panic(err)
	}

	hbd, err = packetEncoder.Encode(packet.Heartbeat, nil)
	if err != nil {
		panic(err)
	}
	hbAck, err = packetEncoder.Encode(packet.HeartbeatAck, nil)
	if err != nil {
		panic(err)
	}
}

func (a *agentImpl) reportChannelSize() {
	chSendCapacity := a.messagesBufferSize - len(a.chSend)
	if chSendCapacity == 0 {
		logger.Zap.Warn("chSend is at maximum capacity")
	}
	for _, mr := range a.metricsReporters {
		if err := mr.ReportGauge(metrics.ChannelCapacity, map[string]string{"channel": "agent_chsend"}, float64(chSendCapacity)); err != nil {
			logger.Zap.Warn("failed to report chSend channel capaacity", zap.Error(err))
		}
	}
}
