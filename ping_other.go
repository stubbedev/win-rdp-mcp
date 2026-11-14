//go:build !windows

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func pingOnce(ctx context.Context, ip net.IP, timeout time.Duration) (time.Duration, error) {
	network, proto, echo := "udp4", 1, icmp.Type(ipv4.ICMPTypeEcho)
	reply := icmp.Type(ipv4.ICMPTypeEchoReply)
	if ip.To4() == nil {
		network, proto, echo, reply = "udp6", 58, ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}
	conn, err := icmp.ListenPacket(network, "")
	if err != nil {
		return 0, fmt.Errorf("opening an unprivileged ICMP socket (%s): %w", network, err)
	}
	defer conn.Close()

	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, fmt.Errorf("setting the ICMP deadline: %w", err)
	}

	id := os.Getpid() & 0xFFFF
	msg := icmp.Message{Type: echo, Body: &icmp.Echo{ID: id, Seq: 1, Data: []byte("win-rdp-mcp")}}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return 0, fmt.Errorf("encoding the echo request: %w", err)
	}
	start := time.Now()
	if _, err := conn.WriteTo(wire, &net.UDPAddr{IP: ip}); err != nil {
		return 0, fmt.Errorf("sending the echo request to %s: %w", ip, err)
	}
	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, fmt.Errorf("no reply from %s: %w", ip, err)
		}
		got, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		if got.Type == reply {
			return time.Since(start), nil
		}
	}
}
