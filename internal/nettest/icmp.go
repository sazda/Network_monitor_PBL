package nettest

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// errPingUnsupported se vraća kada ICMP nije dostupan na platformi.
var errPingUnsupported = errors.New("ICMP ping nije podržan na ovoj platformi")

// MeasureICMP šalje count ICMP echo zahteva ka host-u i vraća statistiku.
// Za razliku od TCP merenja, radi i prema uređajima koji nemaju otvoren
// nijedan TCP port (ruteri, L3 svičevi, štampači...).
func MeasureICMP(ctx context.Context, host string, count int, interval, timeout time.Duration) (protocol.LatencyResult, error) {
	return MeasureICMPProbe(ctx, host, count, interval, timeout, nil)
}

// MeasureICMPProbe je MeasureICMP uz javljanje svakog merenja (onProbe može biti nil).
func MeasureICMPProbe(ctx context.Context, host string, count int, interval, timeout time.Duration, onProbe ProbeFunc) (protocol.LatencyResult, error) {
	res := protocol.LatencyResult{Target: host, Method: "icmp"}
	ip, err := resolveIPv4(ctx, host)
	if err != nil {
		return res, err
	}
	p, err := newPinger()
	if err != nil {
		return res, err
	}
	defer p.Close()
	var last time.Time
	for i := 0; i < count && ctx.Err() == nil; i++ {
		// Razmak se računa od početka prethodnog pinga, pa izgubljeni paketi
		// (koji čekaju do timeout-a) ne produžavaju ukupno trajanje.
		if wait := interval - time.Since(last); i > 0 && wait > 0 {
			select {
			case <-ctx.Done():
				continue
			case <-time.After(wait):
			}
		}
		last = time.Now()
		res.Sent++
		rtt, err := p.Ping(ip, i, timeout)
		if err != nil {
			probeLost(&res, onProbe)
			continue
		}
		probeOK(&res, onProbe, rtt)
	}
	FillLatencyStats(&res)
	return res, nil
}

func resolveIPv4(ctx context.Context, host string) (net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4, nil
		}
		return nil, errors.New("ICMP merenje trenutno podržava samo IPv4")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
	if err != nil {
		return nil, err
	}
	return ips[0].To4(), nil
}

// pinger šalje jedan ICMP echo i vraća RTT.
type pinger interface {
	Ping(ip net.IP, seq int, timeout time.Duration) (time.Duration, error)
	Close()
}
