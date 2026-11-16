//go:build !windows

package nettest

import (
	"errors"
	"math/rand/v2"
	"net"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// Na Linux-u/macOS-u se prvo pokušava „neprivilegovani” ICMP socket
// (udp4; na Linux-u zavisi od net.ipv4.ping_group_range), a zatim raw socket
// (ip4:icmp), koji radi kada agent radi kao root servis.
type unixPinger struct {
	c   *icmp.PacketConn
	raw bool
	id  int
}

func newPinger() (pinger, error) {
	if c, err := icmp.ListenPacket("udp4", "0.0.0.0"); err == nil {
		return &unixPinger{c: c}, nil
	}
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, errors.New("ICMP nije dostupan (agent mora da radi kao root ili podesite net.ipv4.ping_group_range)")
	}
	return &unixPinger{c: c, raw: true, id: rand.IntN(0xffff) + 1}, nil
}

func (p *unixPinger) Close() { p.c.Close() }

func (p *unixPinger) Ping(ip net.IP, seq int, timeout time.Duration) (time.Duration, error) {
	seq &= 0xffff
	msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: p.id, Seq: seq, Data: []byte("network-monitor-ping")}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	var dst net.Addr = &net.UDPAddr{IP: ip}
	if p.raw {
		dst = &net.IPAddr{IP: ip}
	}
	deadline := time.Now().Add(timeout)
	p.c.SetReadDeadline(deadline)
	start := time.Now()
	if _, err := p.c.WriteTo(b, dst); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := p.c.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		rtt := time.Since(start)
		m, err := icmp.ParseMessage(1, buf[:n])
		if err != nil || m.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		echo, ok := m.Body.(*icmp.Echo)
		if !ok || echo.Seq != seq || !sameIP(from, ip) {
			continue
		}
		// Kod udp4 socket-a kernel sam postavlja ID, pa ga ne proveravamo.
		if p.raw && echo.ID != p.id {
			continue
		}
		return rtt, nil
	}
}

func sameIP(a net.Addr, ip net.IP) bool {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.IP.Equal(ip)
	case *net.IPAddr:
		return v.IP.Equal(ip)
	}
	return false
}
