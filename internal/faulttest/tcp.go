package faulttest

import (
	"errors"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"
)

// TCPProxy sits between the code under test and a real dependency (a test
// database, a test Redis) on loopback and faults the transport on command:
// a test toggles each fault at an explicit point, so the scenario is the same
// on every run. It is in-process: no proxy service to run, and it can only
// reach the upstream the test names.
//
//	proxy := faulttest.NewTCPProxy(t, "127.0.0.1:5432")
//	dsn := faulttest.PostgresDSNVia(dsn, proxy.Addr())
//	proxy.Hold()   // the dependency stops answering (latency past any deadline)
//	proxy.Cut()    // every open connection drops
//	proxy.Refuse() // new connections are refused
//	proxy.Heal()   // all of it lifted: the next operation must succeed
type TCPProxy struct {
	upstream string
	addr     string

	mu       sync.Mutex
	ln       net.Listener
	refusing bool
	gate     chan struct{} // closed while traffic flows; open (blocking) while held
	held     chan struct{} // closed once a chunk waits on a hold
	heldOnce bool
	conns    map[*proxyPair]struct{}
	accepted int
	wg       sync.WaitGroup
	closed   bool
}

type proxyPair struct {
	client, upstream net.Conn
	done             chan struct{}
	once             sync.Once
}

func (p *proxyPair) close(reset bool) {
	p.once.Do(func() {
		if reset {
			if tcp, ok := p.client.(*net.TCPConn); ok {
				_ = tcp.SetLinger(0) // RST, not FIN
			}
		}
		_ = p.client.Close()
		_ = p.upstream.Close()
		close(p.done)
	})
}

// NewTCPProxy starts a proxy to upstream on a loopback port, and closes it
// when t ends.
func NewTCPProxy(t testing.TB, upstream string) *TCPProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("faulttest: proxy listen: %v", err)
	}
	flowing := make(chan struct{})
	close(flowing)
	p := &TCPProxy{upstream: upstream, addr: ln.Addr().String(), ln: ln, gate: flowing,
		held: make(chan struct{}), conns: map[*proxyPair]struct{}{}}
	p.wg.Add(1)
	go p.accept(ln)
	t.Cleanup(p.Close)
	return p
}

// Addr is the proxy's address: point the code under test here instead of at
// the upstream.
func (p *TCPProxy) Addr() string { return p.addr }

func (p *TCPProxy) accept(ln net.Listener) {
	defer p.wg.Done()
	for {
		client, err := ln.Accept()
		if err != nil {
			return // listener closed: Refuse or Close
		}
		p.mu.Lock()
		p.accepted++
		p.wg.Add(1)
		p.mu.Unlock()
		go p.connect(client) // a slow upstream dial never stalls the next accept
	}
}

// upstreamDialTimeout bounds the proxy's own dial to the upstream.
const upstreamDialTimeout = 5 * time.Second

// connect dials the upstream for client and starts piping.
func (p *TCPProxy) connect(client net.Conn) {
	defer p.wg.Done()
	upstream, err := net.DialTimeout("tcp", p.upstream, upstreamDialTimeout)
	if err != nil {
		_ = client.Close()
		return
	}
	pair := &proxyPair{client: client, upstream: upstream, done: make(chan struct{})}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		pair.close(false)
		return
	}
	p.conns[pair] = struct{}{}
	p.wg.Add(2)
	p.mu.Unlock()
	go p.pipe(pair, client, upstream)
	go p.pipe(pair, upstream, client)
}

// pipe copies src to dst chunk by chunk, stopping each chunk at the gate
// while the proxy is held.
func (p *TCPProxy) pipe(pair *proxyPair, src, dst net.Conn) {
	defer p.wg.Done()
	defer func() {
		pair.close(false)
		p.mu.Lock()
		delete(p.conns, pair)
		p.mu.Unlock()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !p.wait(pair) {
				return
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return // EOF or error on either side ends the connection pair
		}
	}
}

// wait blocks while the proxy is held, reporting false when the connection
// was closed meanwhile.
func (p *TCPProxy) wait(pair *proxyPair) bool {
	p.mu.Lock()
	gate := p.gate
	select {
	case <-gate:
		p.mu.Unlock()
		return true
	default:
	}
	if !p.heldOnce {
		p.heldOnce = true
		close(p.held)
	}
	p.mu.Unlock()
	select {
	case <-gate:
		return true
	case <-pair.done:
		return false
	}
}

// Hold stops forwarding in both directions: connections stay open, and every
// byte waits until Heal. To the client the dependency is slower than any
// deadline.
func (p *TCPProxy) Hold() {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.gate:
		p.gate = make(chan struct{})
		p.held = make(chan struct{})
		p.heldOnce = false
	default: // already held
	}
}

// Held returns a channel closed once traffic is waiting on the current Hold:
// the call under test has reached the dependency and stalled.
func (p *TCPProxy) Held() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.held
}

// Cut closes every open connection, both sides (a connection lost
// mid-query).
func (p *TCPProxy) Cut() { p.closeAll(false) }

// Reset closes every open connection with a TCP reset.
func (p *TCPProxy) Reset() { p.closeAll(true) }

func (p *TCPProxy) closeAll(reset bool) {
	p.mu.Lock()
	pairs := make([]*proxyPair, 0, len(p.conns))
	for pair := range p.conns {
		pairs = append(pairs, pair)
	}
	p.mu.Unlock()
	for _, pair := range pairs {
		pair.close(reset)
	}
}

// Refuse stops accepting: a new connection is refused at once, as by a
// dependency that is down. Open connections are left alone (Cut them too
// for a full outage).
func (p *TCPProxy) Refuse() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refusing || p.closed {
		return
	}
	p.refusing = true
	_ = p.ln.Close()
}

// Heal lifts every fault: held traffic flows, and a refusing proxy listens
// again on the same address. Connections that were cut stay cut; the next
// operation has to reconnect, which is the recovery a test asserts.
func (p *TCPProxy) Heal() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.gate:
	default:
		close(p.gate)
	}
	if p.refusing && !p.closed {
		ln, err := net.Listen("tcp", p.addr)
		if err != nil {
			return err
		}
		p.ln = ln
		p.refusing = false
		p.wg.Add(1)
		go p.accept(ln)
	}
	return nil
}

// Connections reports how many connections the proxy has accepted.
func (p *TCPProxy) Connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accepted
}

// Close stops the proxy and drops every connection.
func (p *TCPProxy) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	if !p.refusing {
		_ = p.ln.Close()
	}
	select {
	case <-p.gate:
	default:
		close(p.gate)
	}
	p.mu.Unlock()
	p.closeAll(false)
	p.wg.Wait()
}

// PostgresDSNVia points a postgres:// URL DSN at addr, keeping everything
// else.
func PostgresDSNVia(dsn, addr string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", errors.New("faulttest: PostgresDSNVia needs a postgres:// URL")
	}
	u.Host = addr
	return u.String(), nil
}

// PostgresHostPort returns a postgres:// URL DSN's host:port: the upstream
// to proxy.
func PostgresHostPort(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}
