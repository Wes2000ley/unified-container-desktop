//go:build linux

package dnsfilter

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestUDPListenerReturnsFilteredAnswer(t *testing.T) {
	listener, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	settings, resolvers := listenerSettings(t, "ipv4")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveUDP(ctx, listener, settings, resolvers, nil, unexpectedForward(t)) }()
	defer func() {
		cancel()
		_ = listener.Close()
		if err := <-done; !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Errorf("UDP listener exit = %v", err)
		}
	}()

	client, err := net.Dial("udp", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := client.Write(testDNSQuery(dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 1024)
	n, err := client.Read(answer)
	if err != nil {
		t.Fatal(err)
	}
	assertFilteredDNSAnswer(t, answer[:n], dnsmessage.TypeA)
}

func TestTCPListenerReturnsFilteredAnswer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	settings, resolvers := listenerSettings(t, "ipv6")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveTCP(ctx, listener, settings, resolvers, nil, unexpectedForward(t)) }()
	defer func() {
		cancel()
		_ = listener.Close()
		if err := <-done; !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
			t.Errorf("TCP listener exit = %v", err)
		}
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	query := testDNSQuery(dnsmessage.TypeAAAA)
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(query)))
	if err := writeAll(client, append(length[:], query...)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, length[:]); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(client, answer); err != nil {
		t.Fatal(err)
	}
	assertFilteredDNSAnswer(t, answer, dnsmessage.TypeAAAA)
}

func TestServeAddressesReportsReadyOnlyAfterAllListenersStart(t *testing.T) {
	t.Run("all listeners bound", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ready := make(chan struct{}, 1)
		done := make(chan error, 1)
		go func() {
			done <- serveAddresses(ctx, []string{"127.0.0.1:0", "127.0.0.1:0"}, nil, nil, nil, nil, ready)
		}()
		select {
		case <-ready:
		case err := <-done:
			t.Fatalf("listeners exited before readiness: %v", err)
		case <-time.After(3 * time.Second):
			t.Fatal("DNS listeners did not report readiness")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("listeners returned after cancellation: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("DNS listeners did not stop after cancellation")
		}
	})

	t.Run("one listener fails to bind", func(t *testing.T) {
		ready := make(chan struct{}, 1)
		err := serveAddresses(context.Background(), []string{"127.0.0.1:0", "invalid-address"}, nil, nil, nil, nil, ready)
		if err == nil {
			t.Fatal("listener startup succeeded with an invalid address")
		}
		select {
		case <-ready:
			t.Fatal("DNS was reported ready after a listener failed")
		default:
		}
	})

	t.Run("canceled before startup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ready := make(chan struct{}, 1)
		if err := serveAddresses(ctx, []string{"127.0.0.1:0"}, nil, nil, nil, nil, ready); err != nil {
			t.Fatalf("canceled listener startup returned %v", err)
		}
		select {
		case <-ready:
			t.Fatal("DNS was reported ready after cancellation")
		default:
		}
	})
}

func TestExchangeRetriesTruncatedUDPOverTCP(t *testing.T) {
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	tcp, err := net.Listen("tcp", udp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()

	udpDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 4096)
		n, client, err := udp.ReadFrom(buffer)
		if err != nil {
			udpDone <- err
			return
		}
		response, err := truncatedDNSResponse(buffer[:n])
		if err == nil {
			_, err = udp.WriteTo(response, client)
		}
		udpDone <- err
	}()
	tcpDone := make(chan error, 1)
	go func() {
		conn, err := tcp.Accept()
		if err != nil {
			tcpDone <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			tcpDone <- err
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(length[:]))
		if _, err := io.ReadFull(conn, query); err != nil {
			tcpDone <- err
			return
		}
		response := testDNSResponse(query)
		binary.BigEndian.PutUint16(length[:], uint16(len(response)))
		tcpDone <- writeAll(conn, append(length[:], response...))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	answer, err := exchange(ctx, testDNSQuery(dnsmessage.TypeA), false, []string{udp.LocalAddr().String()})
	if err != nil {
		t.Fatal(err)
	}
	var response dnsmessage.Message
	if err := response.Unpack(answer); err != nil {
		t.Fatal(err)
	}
	if response.Header.Truncated {
		t.Fatal("TCP retry returned a truncated answer")
	}
	if err := <-udpDone; err != nil {
		t.Fatalf("UDP upstream response: %v", err)
	}
	if err := <-tcpDone; err != nil {
		t.Fatalf("TCP upstream response: %v", err)
	}
}

func listenerSettings(t *testing.T, inhibition string) (*settingsFile, *resolverFile) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dnsfilter.json")
	if err := WriteConfig(path, Config{Inhibition: inhibition, DefaultNetworkingMode: "dual-stack"}); err != nil {
		t.Fatal(err)
	}
	return &settingsFile{path: path}, &resolverFile{path: filepath.Join(t.TempDir(), "resolv.conf")}
}

func unexpectedForward(t *testing.T) exchangeFunc {
	t.Helper()
	return func(context.Context, []byte, bool, []string) ([]byte, error) {
		t.Error("filtered query unexpectedly reached DNS forwarder")
		return nil, nil
	}
}

func truncatedDNSResponse(query []byte) ([]byte, error) {
	var request dnsmessage.Message
	if err := request.Unpack(query); err != nil {
		return nil, err
	}
	response := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: request.ID, Response: true, Truncated: true},
		Questions: request.Questions,
	}
	return response.Pack()
}

func assertFilteredDNSAnswer(t *testing.T, data []byte, wantType dnsmessage.Type) {
	t.Helper()
	var response dnsmessage.Message
	if err := response.Unpack(data); err != nil {
		t.Fatal(err)
	}
	if !response.Header.Response || response.Header.RCode != dnsmessage.RCodeSuccess || len(response.Answers) != 0 {
		t.Fatalf("unexpected DNS filtered answer: %#v", response)
	}
	if len(response.Questions) != 1 || response.Questions[0].Type != wantType {
		t.Fatalf("unexpected DNS question in response: %#v", response.Questions)
	}
}
