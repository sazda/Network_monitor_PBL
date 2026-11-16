// Package nettest sadrži merenja latencije i propusnosti koja koristi agent.
package nettest

import (
	"context"
	"math"
	"net"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// MeasureLatency meri vreme uspostavljanja TCP konekcije (TCP handshake) prema
// target-u (host:port). TCP handshake je jedan round-trip (SYN -> SYN/ACK) i
// prolazi kroz firewall-ove koji blokiraju ICMP, ali zahteva otvoren port.
func MeasureLatency(ctx context.Context, target string, count int, interval, timeout time.Duration) protocol.LatencyResult {
	return MeasureLatencyProbe(ctx, target, count, interval, timeout, nil)
}

// ProbeFunc se poziva posle svakog pojedinačnog merenja (seq od 1; ok=false
// znači da odgovor nije stigao).
type ProbeFunc func(seq int, rttMS float64, ok bool)

// MeasureLatencyProbe je MeasureLatency uz javljanje svakog merenja (onProbe može biti nil).
func MeasureLatencyProbe(ctx context.Context, target string, count int, interval, timeout time.Duration, onProbe ProbeFunc) protocol.LatencyResult {
	res := protocol.LatencyResult{Target: target, Method: "tcp"}
	var d net.Dialer
	var last time.Time
	for i := 0; i < count; i++ {
		if ctx.Err() != nil {
			break
		}
		// Razmak se računa od početka prethodnog merenja (vidi MeasureICMPProbe).
		if wait := interval - time.Since(last); i > 0 && wait > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
			if ctx.Err() != nil {
				break
			}
		}
		last = time.Now()
		res.Sent++
		dctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		conn, err := d.DialContext(dctx, "tcp", target)
		rtt := time.Since(start)
		cancel()
		if err != nil {
			if ctx.Err() != nil { // prekinuto, nije gubitak
				res.Sent--
				break
			}
			probeLost(&res, onProbe)
			continue
		}
		conn.Close()
		probeOK(&res, onProbe, rtt)
	}
	FillLatencyStats(&res)
	return res
}

// FillLatencyStats računa min/avg/max/jitter/gubitak iz Samples, Sent i Received.
func FillLatencyStats(r *protocol.LatencyResult) {
	if r.Sent > 0 {
		r.LossPct = round3(100 * float64(r.Sent-r.Received) / float64(r.Sent))
	}
	if len(r.Samples) == 0 {
		return
	}
	minV, maxV, sum := math.Inf(1), math.Inf(-1), 0.0
	for _, s := range r.Samples {
		minV = math.Min(minV, s)
		maxV = math.Max(maxV, s)
		sum += s
	}
	jitter := 0.0
	for i := 1; i < len(r.Samples); i++ {
		jitter += math.Abs(r.Samples[i] - r.Samples[i-1])
	}
	if len(r.Samples) > 1 {
		jitter /= float64(len(r.Samples) - 1)
	}
	r.MinMS = round3(minV)
	r.MaxMS = round3(maxV)
	r.AvgMS = round3(sum / float64(len(r.Samples)))
	r.JitterMS = round3(jitter)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// probeOK beleži primljen odgovor (res.Sent je već uvećan).
func probeOK(res *protocol.LatencyResult, onProbe ProbeFunc, rtt time.Duration) {
	ms := round3(float64(rtt.Microseconds()) / 1000)
	res.Received++
	res.Samples = append(res.Samples, ms)
	if onProbe != nil {
		onProbe(res.Sent, ms, true)
	}
}

// probeLost beleži izgubljen paket.
func probeLost(res *protocol.LatencyResult, onProbe ProbeFunc) {
	res.Lost = append(res.Lost, res.Sent)
	if onProbe != nil {
		onProbe(res.Sent, 0, false)
	}
}
