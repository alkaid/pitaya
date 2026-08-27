package pitaya

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	pitayaclient "github.com/alkaid/pitaya/v2/client"
	pitayamessage "github.com/alkaid/pitaya/v2/conn/message"
	"github.com/alkaid/pitaya/v2/session"
	"github.com/grafana/sobek"
	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/metrics"
)

// Response is the type of the response returned by the server
type Response interface{}

var (
	errClientNotConnected = errors.New("pitaya client is not connected")
	errClientDisconnected = errors.New("pitaya client disconnected")
	errRequestIDCollision = errors.New("pitaya request ID collision")
)

type pendingResponseResult struct {
	data []byte
	err  error
}

type pendingResponse struct {
	result chan pendingResponseResult
}

// Client is the pitaya client
// It is used to connect to a pitaya server and send requests and notifies
// It is also used to consume pushes
type Client struct {
	vu                   modules.VU
	client               pitayaclient.PitayaClient
	handshake            *session.HandshakeData
	registrationMu       sync.Mutex
	responses            map[uint]*pendingResponse
	acceptingRequests    bool
	connectionErr        error
	connectionGeneration uint64
	listenerStop         chan struct{}
	pushesMutex          sync.Mutex
	pushes               map[string]chan []byte
	timeout              time.Duration
	metrics              *pitayaMetrics
}

// Connect connects to the server
// addr is the address of the server to connect to
func (c *Client) Connect(addr string) error { //TODO: tls Options
	vuState := c.vu.State()

	if vuState == nil {
		return errors.New("connecting to a pitaya server in the init context is not supported")
	}

	c.registrationMu.Lock()
	if c.acceptingRequests {
		c.registrationMu.Unlock()
		return errors.New("pitaya client is already connected")
	}
	c.stopListenerLocked()
	c.connectionErr = errClientNotConnected
	c.connectionGeneration++
	generation := c.connectionGeneration
	c.client.OnDisconnected(func(reason pitayaclient.CloseReason) {
		c.handleDisconnected(generation, reason)
	})
	err := c.client.ConnectTo(addr)
	if err != nil {
		c.connectionErr = err
		c.registrationMu.Unlock()
		return err
	}

	stop := make(chan struct{})
	c.listenerStop = stop
	c.acceptingRequests = true
	c.connectionErr = nil
	channel := c.client.MsgChannel()
	c.registrationMu.Unlock()
	go c.listen(channel, stop)

	return nil
}

// IsConnected returns true if the client is connected to the server
func (c *Client) IsConnected() bool {
	return c.client.ConnectedStatus()
}

// ConsumePush will return a promise that will be resolved when a push is received on the given route.
// The promise will be rejected if the timeout is reached before a push is received.
// The promise will be resolved with the push data.
func (c *Client) ConsumePush(route string, timeoutMs int) *sobek.Promise {
	promise, resolve, reject := c.makeHandledPromise()
	ch := c.getPushChannelForRoute(route)
	go func() {
		select {
		case data := <-ch:
			var ret Response
			if err := json.Unmarshal(data, &ret); err != nil {
				err = fmt.Errorf("Error unmarshaling response: %s", err)
				reject(err)
				return
			}
			resolve(ret)
			return
		case <-time.After(time.Duration(timeoutMs) * time.Millisecond):
			reject(fmt.Errorf("Timeout waiting for push on route %s", route))
			return
		}
	}()
	return promise
}

// Notify sends a notify to the server
// route is the route to send the notify to
// msg is the message to send
// returns an error if the notify could not be sent
func (c *Client) Notify(route string, msg interface{}) error {
	m := msg
	if m == nil {
		m = map[string]interface{}{}
	}
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}

	return c.client.SendNotify(route, data)
}

// Request sends a request to the server
// route is the route to send the request to
// msg is the message to send
// returns a promise that will be resolved when the response is received
// the promise will be rejected if the timeout is reached before a response is received
func (c *Client) Request(route string, msg interface{}) *sobek.Promise { // TODO: add custom timeout
	m := msg
	if m == nil {
		m = map[string]interface{}{}
	}
	promise, resolve, reject := c.makeHandledPromise()
	data, err := json.Marshal(m)
	if err != nil {
		reject(err)
		return promise
	}

	timeNow := time.Now()
	slot := &pendingResponse{result: make(chan pendingResponseResult, 1)}
	mid, err, protocolFailure := c.sendAndRegister(route, data, slot)
	if err != nil {
		if protocolFailure {
			c.client.Disconnect(pitayaclient.CloseReasonFatal)
		}
		c.pushRequestMetrics(route, time.Since(timeNow), false, false)
		reject(err)
		return promise
	}

	ctx := c.vu.Context()
	go func() {
		timer := time.NewTimer(c.timeout)
		defer timer.Stop()
		defer c.removePendingResponse(mid, slot)

		var ctxDone <-chan struct{}
		if ctx != nil {
			ctxDone = ctx.Done()
		}
		select {
		case result := <-slot.result:
			if result.err != nil {
				c.pushRequestMetrics(route, time.Since(timeNow), false, false)
				reject(result.err)
				return
			}
			c.pushRequestMetrics(route, time.Since(timeNow), true, false)
			var ret Response
			if err := json.Unmarshal(result.data, &ret); err != nil {
				resolve(result.data)
				return
			}
			resolve(ret)
		case <-timer.C:
			c.pushRequestMetrics(route, time.Since(timeNow), false, true)
			reject(fmt.Errorf("Timeout waiting for response on route %s", route))
		case <-ctxDone:
			c.pushRequestMetrics(route, time.Since(timeNow), false, false)
			reject(ctx.Err())
		}
	}()
	return promise
}

func (c *Client) sendAndRegister(route string, data []byte, slot *pendingResponse) (uint, error, bool) {
	c.registrationMu.Lock()
	defer c.registrationMu.Unlock()

	if !c.acceptingRequests {
		if c.connectionErr != nil {
			return 0, c.connectionErr, false
		}
		return 0, errClientNotConnected, false
	}

	mid, err := c.client.SendRequest(route, data)
	if err != nil {
		return mid, err, false
	}
	if c.responses == nil {
		c.responses = make(map[uint]*pendingResponse)
	}
	if _, exists := c.responses[mid]; exists {
		err = fmt.Errorf("%w: %d", errRequestIDCollision, mid)
		c.acceptingRequests = false
		c.connectionErr = err
		c.stopListenerLocked()
		c.failAllPendingLocked(err)
		return mid, err, true
	}
	c.responses[mid] = slot
	return mid, nil, false
}

func (c *Client) removePendingResponse(id uint, slot *pendingResponse) {
	c.registrationMu.Lock()
	defer c.registrationMu.Unlock()
	if c.responses[id] == slot {
		delete(c.responses, id)
	}
}

func (c *Client) pushRequestMetrics(route string, responseTime time.Duration, success bool, timeout bool) error {
	state := c.vu.State()
	if state == nil {
		return errors.New("invalid state")
	}

	ctx := c.vu.Context()
	if ctx == nil {
		return errors.New("invalid context")
	}

	metrics.PushIfNotDone(ctx, state.Samples, metrics.Sample{
		TimeSeries: metrics.TimeSeries{
			Metric: c.metrics.RequestResponseTime,
			Tags: c.metrics.TagsAndMeta.Tags.WithTagsFromMap(state.Options.RunTags).WithTagsFromMap(
				map[string]string{
					"route":   route,
					"success": fmt.Sprintf("%t", success),
				}),
		},
		Value: metrics.D(responseTime),
		Time:  time.Now(),
	})

	value := 0
	if timeout {
		value = 1
	}

	metrics.PushIfNotDone(ctx, state.Samples, metrics.Sample{
		TimeSeries: metrics.TimeSeries{
			Metric: c.metrics.TimeoutRequests,
			Tags: c.metrics.TagsAndMeta.Tags.WithTagsFromMap(state.Options.RunTags).WithTagsFromMap(
				map[string]string{
					"route": route,
				}),
		},
		Value: float64(value),
		Time:  time.Now(),
	})

	return nil
}

// Disconnect disconnects from the server
func (c *Client) Disconnect() {
	c.failConnection(errClientDisconnected)
	c.client.Disconnect(pitayaclient.CloseReasonManual)
}

func (c *Client) handleDisconnected(generation uint64, reason pitayaclient.CloseReason) {
	c.registrationMu.Lock()
	defer c.registrationMu.Unlock()
	if generation != c.connectionGeneration {
		return
	}
	c.failConnectionLocked(fmt.Errorf("%w (reason %d)", errClientDisconnected, reason))
}

func (c *Client) failConnection(err error) {
	c.registrationMu.Lock()
	defer c.registrationMu.Unlock()
	c.failConnectionLocked(err)
}

func (c *Client) failConnectionLocked(err error) {
	c.acceptingRequests = false
	if c.connectionErr == nil || !errors.Is(c.connectionErr, errRequestIDCollision) {
		c.connectionErr = err
	}
	c.stopListenerLocked()
	c.failAllPendingLocked(err)
}

func (c *Client) stopListenerLocked() {
	if c.listenerStop != nil {
		close(c.listenerStop)
		c.listenerStop = nil
	}
}

func (c *Client) failAllPendingLocked(err error) {
	for id, slot := range c.responses {
		delete(c.responses, id)
		select {
		case slot.result <- pendingResponseResult{err: err}:
		default:
		}
	}
}

func (c *Client) listen(channel <-chan *pitayamessage.Message, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case m, ok := <-channel:
			if !ok {
				return
			}
			switch m.Type {
			case pitayamessage.Response:
				c.deliverResponse(m.ID, m.Data)
			case pitayamessage.Push:
				ch := c.getPushChannelForRoute(m.Route)
				select {
				case ch <- m.Data:
				default:
				}
			default:
				c.debugDroppedMessage("unsupported_message_type")
			}
		}
	}
}

func (c *Client) deliverResponse(id uint, data []byte) {
	c.registrationMu.Lock()
	slot := c.responses[id]
	if slot == nil {
		c.registrationMu.Unlock()
		c.debugDroppedMessage("unknown_or_late_response")
		return
	}
	select {
	case slot.result <- pendingResponseResult{data: data}:
		c.registrationMu.Unlock()
	default:
		c.registrationMu.Unlock()
		c.debugDroppedMessage("duplicate_response")
	}
}

func (c *Client) debugDroppedMessage(reason string) {
	state := c.vu.State()
	if state != nil && state.Logger != nil {
		state.Logger.WithField("reason", reason).Debug("dropping pitaya client message")
	}
}

func (c *Client) getPushChannelForRoute(route string) chan []byte {
	c.pushesMutex.Lock()
	defer c.pushesMutex.Unlock()
	if c.pushes == nil {
		c.pushes = make(map[string]chan []byte)
	}
	if _, ok := c.pushes[route]; !ok {
		c.pushes[route] = make(chan []byte, 1)
	}

	return c.pushes[route]
}

// makeHandledPromise will create a promise and return its resolve and reject methods,
// wrapped in such a way that it will block the eventloop from exiting before they are
// called even if the promise isn't resolved by the time the current script ends executing.
func (c *Client) makeHandledPromise() (*sobek.Promise, func(interface{}), func(interface{})) {
	runtime := c.vu.Runtime()
	callback := c.vu.RegisterCallback()
	p, resolve, reject := runtime.NewPromise()

	return p, func(i interface{}) {
			// more stuff
			callback(func() error {
				resolve(i)
				return nil
			})
		}, func(i interface{}) {
			// more stuff
			callback(func() error {
				reject(i)
				return nil
			})
		}
}
