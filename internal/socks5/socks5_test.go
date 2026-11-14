package socks5

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestSOCKS5TCPConnectAndUDPAssociate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := &Server{
		ListenAddr: "127.0.0.1:0",
	}
	go func() {
		_ = srv.Start(ctx)
	}()

	var socksAddr string
	for i := 0; i < 50; i++ {
		if a := srv.Addr(); a != nil {
			socksAddr = a.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if socksAddr == "" {
		t.Fatal("socks5 server did not start")
	}

	// --- 1. Test TCP CONNECT ---
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		c, err := echoLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()

	echoTCPAddr := echoLn.Addr().(*net.TCPAddr)
	conn, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Greeting
	_, _ = conn.Write([]byte{0x05, 0x01, 0x00})
	greetResp := make([]byte, 2)
	if _, err := io.ReadFull(conn, greetResp); err != nil || greetResp[1] != 0x00 {
		t.Fatalf("greeting failed: %v %x", err, greetResp)
	}

	// CONNECT request
	req := make([]byte, 10)
	req[0] = 0x05
	req[1] = 0x01 // CONNECT
	req[2] = 0x00
	req[3] = 0x01 // IPv4
	copy(req[4:8], echoTCPAddr.IP.To4())
	binary.BigEndian.PutUint16(req[8:10], uint16(echoTCPAddr.Port))
	_, _ = conn.Write(req)

	rep := make([]byte, 10)
	if _, err := io.ReadFull(conn, rep); err != nil || rep[1] != 0x00 {
		t.Fatalf("connect reply failed: %v %x", err, rep)
	}

	_, _ = conn.Write([]byte("hello-leigod-tcp"))
	buf := make([]byte, 16)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello-leigod-tcp" {
		t.Fatalf("tcp echo mismatch: %q (%v)", string(buf), err)
	}

	// --- 2. Test UDP ASSOCIATE ---
	udpEcho, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		b := make([]byte, 1024)
		for {
			n, raddr, err := udpEcho.ReadFromUDP(b)
			if err != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(append([]byte("udp:"), b[:n]...), raddr)
		}
	}()

	ctrlConn, err := net.Dial("tcp", socksAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer ctrlConn.Close()

	_, _ = ctrlConn.Write([]byte{0x05, 0x01, 0x00})
	if _, err := io.ReadFull(ctrlConn, greetResp); err != nil {
		t.Fatal(err)
	}

	// UDP ASSOCIATE request (0.0.0.0:0)
	udpReq := []byte{0x05, 0x03, 0x00, 0x01, 0, 0, 0, 0, 0, 0}
	_, _ = ctrlConn.Write(udpReq)
	if _, err := io.ReadFull(ctrlConn, rep); err != nil || rep[1] != 0x00 {
		t.Fatalf("udp associate reply failed: %v %x", err, rep)
	}
	relayIP := net.IP(rep[4:8])
	relayPort := int(binary.BigEndian.Uint16(rep[8:10]))
	relayUDPAddr := &net.UDPAddr{IP: relayIP, Port: relayPort}

	cUDP, err := net.DialUDP("udp4", nil, relayUDPAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer cUDP.Close()

	targetUDPAddr := udpEcho.LocalAddr().(*net.UDPAddr)
	pkt := buildUDPDatagram(targetUDPAddr, []byte("game-packet"))
	_ = cUDP.SetDeadline(time.Now().Add(time.Second))
	if _, err := cUDP.Write(pkt); err != nil {
		t.Fatal(err)
	}

	respPkt := make([]byte, 1024)
	n, err := cUDP.Read(respPkt)
	if err != nil {
		t.Fatal(err)
	}
	_, data, err := parseUDPDatagram(respPkt[:n])
	if err != nil || string(data) != "udp:game-packet" {
		t.Fatalf("udp echo mismatch: %q (%v)", string(data), err)
	}
}
