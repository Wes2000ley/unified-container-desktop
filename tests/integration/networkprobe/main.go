package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		panic("use info, dns NAME, http URL, udp ADDRESS, or serve")
	}
	switch os.Args[1] {
	case "info":
		addresses, err := net.InterfaceAddrs()
		if err != nil {
			panic(err)
		}
		out := map[string]any{"addresses": addresses}
		for _, name := range []string{"/etc/resolv.conf", "/etc/hosts", "/proc/net/route"} {
			if data, err := os.ReadFile(name); err == nil {
				out[name] = string(data)
			}
		}
		json.NewEncoder(os.Stdout).Encode(out)
	case "dns":
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		addresses, err := net.DefaultResolver.LookupHost(ctx, os.Args[2])
		if err != nil {
			panic(err)
		}
		json.NewEncoder(os.Stdout).Encode(addresses)
	case "http":
		client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 8 * time.Second}
		response, err := client.Get(os.Args[2])
		if err != nil {
			panic(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		if err != nil {
			panic(err)
		}
		fmt.Printf("%d %s\n", response.StatusCode, body)
	case "udp":
		conn, err := net.DialTimeout("udp", os.Args[2], 8*time.Second)
		if err != nil {
			panic(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(8 * time.Second))
		if _, err := conn.Write([]byte("UCD_UDP_PROOF")); err != nil {
			panic(err)
		}
		b := make([]byte, 1024)
		n, err := conn.Read(b)
		if err != nil {
			panic(err)
		}
		fmt.Println(string(b[:n]))
	case "serve":
		packet, err := net.ListenPacket("udp", ":5001")
		if err != nil {
			panic(err)
		}
		go func() {
			for {
				b := make([]byte, 1024)
				n, a, err := packet.ReadFrom(b)
				if err != nil {
					return
				}
				packet.WriteTo(b[:n], a)
			}
		}()
		fmt.Println("UCD_NETWORK_READY")
		if err := http.ListenAndServe(":5000", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "UCD_HTTP_PROOF") })); err != nil {
			panic(err)
		}
	default:
		panic("unknown command")
	}
}
