package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"sync"
	"time"
)

var wsUpgradeRegex = regexp.MustCompile(`(?i)upgrade:\s*websocket`)

// WSGuardProxy listens on listenAddr (e.g. 0.0.0.0:5588) and forwards only valid
// WebSocket upgrade connections to targetAddr (e.g. 10.254.254.2:5588).
// Plain HTTP requests or port scans are dropped immediately so the upstream
// Leigod websocketpp server never crashes on non-WebSocket traffic.
type WSGuardProxy struct {
	ListenAddr   string
	TargetAddr   string
	SniffTimeout time.Duration
	DialTimeout  time.Duration
	Logf         func(string, ...any)

	mu       sync.Mutex
	listener net.Listener
}

// Start begins accepting connections until ctx is cancelled.
func (p *WSGuardProxy) Start(ctx context.Context) error {
	if p.SniffTimeout <= 0 {
		p.SniffTimeout = 2 * time.Second
	}
	if p.DialTimeout <= 0 {
		p.DialTimeout = 3 * time.Second
	}

	ln, err := net.Listen("tcp", p.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen wsguard %s: %w", p.ListenAddr, err)
	}
	p.mu.Lock()
	p.listener = ln
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go p.handleConn(ctx, conn)
	}
}

// Addr returns the bound listener address (useful in tests with :0).
func (p *WSGuardProxy) Addr() net.Addr {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener == nil {
		return nil
	}
	return p.listener.Addr()
}

func (p *WSGuardProxy) handleConn(ctx context.Context, clientConn net.Conn) {
	defer clientConn.Close()

	_ = clientConn.SetReadDeadline(time.Now().Add(p.SniffTimeout))
	buf := make([]byte, 4096)
	n, err := clientConn.Read(buf)
	if err != nil || n == 0 {
		return
	}
	_ = clientConn.SetReadDeadline(time.Time{})

	initial := buf[:n]
	if !IsWebSocketHandshake(initial) {
		if p.Logf != nil {
			p.Logf("wsguard: dropped non-WebSocket connection from %s", clientConn.RemoteAddr())
		}
		return
	}

	dialer := net.Dialer{Timeout: p.DialTimeout}
	backendConn, err := dialer.DialContext(ctx, "tcp", p.TargetAddr)
	if err != nil {
		if p.Logf != nil {
			p.Logf("wsguard: dial %s failed: %v", p.TargetAddr, err)
		}
		return
	}
	defer backendConn.Close()

	if _, err := backendConn.Write(initial); err != nil {
		return
	}

	RelayTCP(clientConn, backendConn)
}

// IsWebSocketHandshake returns true if the initial HTTP request headers contain Upgrade: websocket.
func IsWebSocketHandshake(data []byte) bool {
	if !bytes.HasPrefix(data, []byte("GET ")) && !bytes.Contains(data, []byte("HTTP/1.")) {
		return false
	}
	return wsUpgradeRegex.Match(data)
}

// TCPProxy is a simple transparent TCP port forwarder (e.g., 127.0.0.1:15588 -> 10.254.254.2:1080).
type TCPProxy struct {
	ListenAddr  string
	TargetAddr  string
	DialTimeout time.Duration
	Logf        func(string, ...any)

	mu       sync.Mutex
	listener net.Listener
}

// Start runs the TCP forwarder until ctx is cancelled.
func (p *TCPProxy) Start(ctx context.Context) error {
	if p.DialTimeout <= 0 {
		p.DialTimeout = 3 * time.Second
	}
	ln, err := net.Listen("tcp", p.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen tcp proxy %s: %w", p.ListenAddr, err)
	}
	p.mu.Lock()
	p.listener = ln
	p.mu.Unlock()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
		go func(c net.Conn) {
			defer c.Close()
			dialer := net.Dialer{Timeout: p.DialTimeout}
			up, err := dialer.DialContext(ctx, "tcp", p.TargetAddr)
			if err != nil {
				return
			}
			defer up.Close()
			RelayTCP(c, up)
		}(conn)
	}
}

// RelayTCP copies data bidirectionally between two TCP connections.
func RelayTCP(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		if tc, ok := b.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		if tc, ok := a.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	wg.Wait()
}
