package console

import (
	"math"
	"slices"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// Statistika merenja latencije / pinga (za panel uživo i izveštaj).

// latStats je sažetak niza merenja.
type latStats struct {
	Sent     int     `json:"sent"`
	Received int     `json:"received"`
	Lost     int     `json:"lost"`
	LossPct  float64 `json:"loss_pct"`
	MinMS    float64 `json:"min_ms"`
	AvgMS    float64 `json:"avg_ms"`
	MaxMS    float64 `json:"max_ms"`
	JitterMS float64 `json:"jitter_ms"`
	Median   float64 `json:"median_ms"`
	P95      float64 `json:"p95_ms"`
	LastMS   float64 `json:"last_ms"`
	LastLost bool    `json:"last_lost,omitempty"`
	Outages  int     `json:"outages"` // broj prekida (niz uzastopnih izgubljenih paketa)
	Longest  *outage `json:"longest,omitempty"`
}

// outage je niz uzastopnih izgubljenih paketa.
type outage struct {
	FromSeq int       `json:"from_seq"`
	ToSeq   int       `json:"to_seq"`
	Count   int       `json:"count"`
	From    time.Time `json:"from,omitzero"`
	To      time.Time `json:"to,omitzero"`
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func computeLatStats(probes []liveProbe) latStats {
	s := latStats{Sent: len(probes)}
	var rtts []float64
	jitter, prev, havePrev := 0.0, 0.0, false
	for _, p := range probes {
		if p.Lost {
			continue
		}
		rtts = append(rtts, p.RTT)
		if havePrev {
			jitter += math.Abs(p.RTT - prev)
		}
		prev, havePrev = p.RTT, true
	}
	s.Received = len(rtts)
	s.Lost = s.Sent - s.Received
	if s.Sent > 0 {
		s.LossPct = round2(100 * float64(s.Lost) / float64(s.Sent))
		last := probes[len(probes)-1]
		s.LastMS, s.LastLost = last.RTT, last.Lost
	}
	out := outages(probes)
	s.Outages = len(out)
	for i := range out {
		if s.Longest == nil || out[i].Count > s.Longest.Count {
			s.Longest = &out[i]
		}
	}
	if len(rtts) == 0 {
		return s
	}
	sum := 0.0
	for _, v := range rtts {
		sum += v
	}
	if len(rtts) > 1 {
		jitter /= float64(len(rtts) - 1)
	}
	sorted := slices.Clone(rtts)
	slices.Sort(sorted)
	q := func(f float64) float64 { return sorted[min(len(sorted)-1, int(f*float64(len(sorted))))] }
	s.MinMS, s.MaxMS = sorted[0], sorted[len(sorted)-1]
	s.AvgMS = round2(sum / float64(len(rtts)))
	s.JitterMS = round2(jitter)
	s.Median, s.P95 = q(0.5), q(0.95)
	return s
}

// outages vraća nizove uzastopnih izgubljenih paketa.
func outages(probes []liveProbe) []outage {
	var out []outage
	for i := 0; i < len(probes); i++ {
		if !probes[i].Lost {
			continue
		}
		j := i
		for j+1 < len(probes) && probes[j+1].Lost {
			j++
		}
		o := outage{FromSeq: probes[i].Seq, ToSeq: probes[j].Seq, Count: j - i + 1, From: probes[i].At, To: probes[j].At}
		out = append(out, o)
		i = j
	}
	return out
}

// downsample sažima niz merenja na najviše max tačaka (za grafikon): svaka
// tačka je prosek grupe uzastopnih paketa, uz najveće vreme i broj izgubljenih.
func downsample(probes []liveProbe, max int) []liveProbe {
	if len(probes) <= max {
		return slices.Clone(probes)
	}
	size := (len(probes) + max - 1) / max
	out := make([]liveProbe, 0, max)
	for i := 0; i < len(probes); i += size {
		g := probes[i:min(i+size, len(probes))]
		b := liveProbe{Seq: g[0].Seq, SeqEnd: g[len(g)-1].Seq}
		sum, n := 0.0, 0
		for _, p := range g {
			if p.Lost {
				b.LostN++
				continue
			}
			sum += p.RTT
			n++
			b.MaxRTT = math.Max(b.MaxRTT, p.RTT)
		}
		if n > 0 {
			b.RTT = round2(sum / float64(n))
		} else {
			b.Lost = true
		}
		out = append(out, b)
	}
	return out
}

// probesFromResult rekonstruiše niz paketa iz konačnog rezultata agenta.
// Ako agent ne javlja redne brojeve izgubljenih paketa (stariji agenti),
// izgubljeni se stavljaju na kraj, a lostSeq je false.
func probesFromResult(res *protocol.LatencyResult, start time.Time) (probes []liveProbe, lostSeq bool) {
	lost := map[int]bool{}
	for _, s := range res.Lost {
		lost[s] = true
	}
	lostSeq = len(res.Lost) == res.Sent-res.Received
	probes = make([]liveProbe, 0, res.Sent)
	si := 0
	for seq := 1; seq <= res.Sent; seq++ {
		p := liveProbe{Seq: seq}
		if seq-1 < len(res.TimesMS) && !start.IsZero() {
			p.At = start.Add(time.Duration(res.TimesMS[seq-1]) * time.Millisecond)
		}
		if lost[seq] || (!lostSeq && si >= len(res.Samples)) || si >= len(res.Samples) {
			p.Lost = true
		} else {
			p.RTT = res.Samples[si]
			si++
		}
		probes = append(probes, p)
	}
	return probes, lostSeq
}
