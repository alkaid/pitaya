package acceptor

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/mailgun/proxyproto"
)

const (
	DefaultProxyProtocolHeaderTimeout = 5 * time.Second
	// DefaultProxyProtocolMaxPending bounds connections which have been accepted
	// but have not completed PROXY header parsing and handoff.
	DefaultProxyProtocolMaxPending = 64
)

var ErrProxyProtocolSourceAddress = errors.New("proxy protocol header has no source address")

// Listener is used to wrap an underlying listener,
// whose connections may be using the HAProxy Proxy Protocol.
// If the connection is using the protocol, the RemoteAddr() will return
// the correct client address.
type ProxyProtocolListener struct {
	net.Listener
	ProxyProtocolEnabled bool
	HeaderTimeout        time.Duration

	stateOnce sync.Once
	pumpOnce  sync.Once
	closeOnce sync.Once
	done      chan struct{}
	results   chan proxyAcceptResult
	pumpDone  chan struct{}
	slots     chan struct{}

	pendingMu sync.Mutex
	pending   map[net.Conn]struct{}
	parserWG  sync.WaitGroup
	closeErr  error
}

type proxyAcceptResult struct {
	conn net.Conn
	err  error
}

// Accept returns the next connection after any PROXY header has been parsed.
// A single slow header cannot block the underlying listener from accepting
// other peers; the parser admission is bounded and excess peers are rejected.
func (p *ProxyProtocolListener) Accept() (net.Conn, error) {
	if !p.ProxyProtocolEnabled {
		return p.Listener.Accept()
	}
	p.startPump()
	result, ok := <-p.results
	if !ok {
		return nil, net.ErrClosed
	}
	return result.conn, result.err
}

func (p *ProxyProtocolListener) Close() error {
	p.initState()
	p.closeOnce.Do(func() {
		close(p.done)
		p.closeErr = p.Listener.Close()
		p.pendingMu.Lock()
		for conn := range p.pending {
			_ = conn.Close()
		}
		p.pendingMu.Unlock()
		p.startPump()
		<-p.pumpDone
	})
	return p.closeErr
}

func (p *ProxyProtocolListener) initState() {
	p.stateOnce.Do(func() {
		p.done = make(chan struct{})
		p.results = make(chan proxyAcceptResult)
		p.pumpDone = make(chan struct{})
		p.slots = make(chan struct{}, DefaultProxyProtocolMaxPending)
		p.pending = make(map[net.Conn]struct{})
	})
}

func (p *ProxyProtocolListener) startPump() {
	p.initState()
	p.pumpOnce.Do(func() {
		select {
		case <-p.done:
			close(p.results)
			close(p.pumpDone)
		default:
			go p.pump()
		}
	})
}

func (p *ProxyProtocolListener) pump() {
	defer close(p.pumpDone)
	defer func() {
		p.parserWG.Wait()
		close(p.results)
	}()

	for {
		conn, err := p.Listener.Accept()
		if err != nil {
			select {
			case <-p.done:
				return
			case p.results <- proxyAcceptResult{err: err}:
				return
			}
		}

		select {
		case p.slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}

		if !p.trackPending(conn) {
			<-p.slots
			_ = conn.Close()
			return
		}
		p.parserWG.Add(1)
		go p.parseAndPublish(conn)
	}
}

func (p *ProxyProtocolListener) trackPending(conn net.Conn) bool {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	select {
	case <-p.done:
		return false
	default:
		p.pending[conn] = struct{}{}
		return true
	}
}

func (p *ProxyProtocolListener) parseAndPublish(conn net.Conn) {
	defer p.parserWG.Done()
	defer func() { <-p.slots }()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pending, conn)
		p.pendingMu.Unlock()
	}()

	parsed := &Conn{Conn: conn}
	var result proxyAcceptResult
	if p.ProxyProtocolEnabled {
		if err := parsed.checkPrefix(p.headerTimeout()); err != nil {
			_ = conn.Close()
			result.err = &proxyProtocolError{err: err}
		} else {
			result.conn = parsed
		}
	} else {
		result.conn = parsed
	}

	select {
	case p.results <- result:
	case <-p.done:
		if result.conn != nil {
			_ = result.conn.Close()
		}
	}
}

func (p *ProxyProtocolListener) headerTimeout() time.Duration {
	if p.HeaderTimeout > 0 {
		return p.HeaderTimeout
	}
	return DefaultProxyProtocolHeaderTimeout
}

type proxyProtocolError struct {
	err error
}

func (e *proxyProtocolError) Error() string {
	return fmt.Sprintf("reject proxy protocol connection: %v", e.err)
}

func (e *proxyProtocolError) Unwrap() error {
	return e.err
}

func isProxyProtocolError(err error) bool {
	var proxyErr *proxyProtocolError
	return errors.As(err, &proxyErr)
}

// Conn is used to wrap and underlying connection which
// may be speaking the Proxy Protocol. If it is, the RemoteAddr() will
// return the address of the client instead of the proxy address.
type Conn struct {
	net.Conn
	dstAddr *net.Addr
	srcAddr *net.Addr
}

func (p *Conn) LocalAddr() net.Addr {
	if p.dstAddr != nil {
		return *p.dstAddr
	}
	return p.Conn.LocalAddr()
}

// RemoteAddr returns the address of the client if the proxy
// protocol is being used, otherwise just returns the address of
// the socket peer. If there is an error parsing the header, the
// address of the client is not returned, and the socket is closed.
// Once implication of this is that the call could block if the
// client is slow. Using a Deadline is recommended if this is called
// before Read()
func (p *Conn) RemoteAddr() net.Addr {
	if p.srcAddr != nil {
		return *p.srcAddr
	}
	return p.Conn.RemoteAddr()
}

func (p *Conn) checkPrefix(timeout time.Duration) error {
	if err := p.Conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set proxy protocol header deadline: %w", err)
	}
	h, err := proxyproto.ReadHeader(p)
	if err != nil {
		return err
	}
	if h.Source == nil {
		return ErrProxyProtocolSourceAddress
	}
	if err := p.Conn.SetReadDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear proxy protocol header deadline: %w", err)
	}

	p.srcAddr = &h.Source
	p.dstAddr = &h.Destination
	return nil
}
