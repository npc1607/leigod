package bridge

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWSGuardProxyBlocksPlainHTTPAndAllowsWebSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Mock backend WebSocket server
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendLn.Close()

	go func() {
		for {
			c, err := backendLn.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 1024)
				n, _ := conn.Read(buf)
				if strings.Contains(string(buf[:n]), "Upgrade: websocket") {
					_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n\r\npong"))
				}
			}(c)
		}
	}()

	proxy := &WSGuardProxy{
		ListenAddr:   "127.0.0.1:0",
		TargetAddr:   backendLn.Addr().String(),
		SniffTimeout: 500 * time.Millisecond,
	}
	go func() {
		_ = proxy.Start(ctx)
	}()

	// Wait for proxy listener
	var proxyAddr string
	for i := 0; i < 50; i++ {
		if addr := proxy.Addr(); addr != nil {
			proxyAddr = addr.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if proxyAddr == "" {
		t.Fatal("proxy did not start")
	}

	// 1. Plain HTTP request should be dropped (EOF, 0 bytes returned)
	c1, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c1.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"))
	resp1, _ := io.ReadAll(c1)
	_ = c1.Close()
	if len(resp1) != 0 {
		t.Fatalf("expected plain HTTP to be dropped, got %q", string(resp1))
	}

	// 2. WebSocket Upgrade request should be forwarded
	c2, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c2.Write([]byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"))
	resp2, err := io.ReadAll(c2)
	_ = c2.Close()
	if err != nil || !strings.Contains(string(resp2), "101 Switching Protocols") {
		t.Fatalf("expected WebSocket handshake response, got %q (%v)", string(resp2), err)
	}
}

func TestUDPRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Mock Leigod UDP 6066 server inside namespace
	backendPC, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backendPC.Close()

	go func() {
		buf := make([]byte, 1024)
		for {
			n, addr, err := backendPC.ReadFrom(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == "<NBOX> WHO" {
				_, _ = backendPC.WriteTo([]byte("model=Jupiter, sn=02:11:22:33:44:55, ip=192.168.1.3"), addr)
			}
		}
	}()

	relay := &UDPRelay{
		ListenAddr:   "127.0.0.1:0",
		TargetAddr:   backendPC.LocalAddr().String(),
		ReplyTimeout: time.Second,
	}
	go func() {
		_ = relay.Start(ctx)
	}()

	var relayAddr string
	for i := 0; i < 50; i++ {
		if addr := relay.Addr(); addr != nil {
			relayAddr = addr.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if relayAddr == "" {
		t.Fatal("udp relay did not start")
	}

	clientConn, err := net.Dial("udp4", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	_ = clientConn.SetDeadline(time.Now().Add(time.Second))
	if _, err := clientConn.Write([]byte("<NBOX> WHO")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 1024)
	n, err := clientConn.Read(reply)
	if err != nil || !strings.Contains(string(reply[:n]), "model=Jupiter") {
		t.Fatalf("unexpected UDP reply: %q (%v)", string(reply[:n]), err)
	}
}
