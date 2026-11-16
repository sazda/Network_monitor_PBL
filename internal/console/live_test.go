package console

import (
	"errors"
	"testing"

	"github.com/sazda/network-monitor/internal/protocol"
)

func TestFinishLatencyProbes(t *testing.T) {
	lt := newLiveTracker()

	// Novi agent: zna se koji je paket izgubljen (2. od 4).
	tst := lt.start(liveView{Kind: "latency"}, "")
	lt.progress(tst, protocol.Progress{Phase: "probe", Seq: 1, RTTMS: 1.5})
	lt.finishLatency(tst, &protocol.LatencyResult{Sent: 4, Received: 3, Samples: []float64{1.5, 2, 3}, Lost: []int{2}, LossPct: 25}, nil)
	v := lt.snapshot()[0]
	if !v.LostSeq || len(v.Probes) != 4 || !v.Probes[1].Lost || v.Probes[2].RTT != 2 || v.Probes[3].RTT != 3 || v.Phase != "done" {
		t.Fatalf("novi agent: %+v", v)
	}

	// Stari agent: samo broj izgubljenih; pozicije nisu poznate.
	tst = lt.start(liveView{Kind: "latency"}, "")
	lt.finishLatency(tst, &protocol.LatencyResult{Sent: 3, Received: 2, Samples: []float64{1, 2}}, nil)
	v = lt.snapshot()[0]
	if v.LostSeq || v.Live || len(v.Probes) != 3 || v.Probes[0].RTT != 1 || !v.Probes[2].Lost {
		t.Fatalf("stari agent: %+v", v)
	}

	// Greška.
	tst = lt.start(liveView{Kind: "latency"}, "")
	lt.finishLatency(tst, &protocol.LatencyResult{}, errors.New("nedostupno"))
	if v = lt.snapshot()[0]; v.Phase != "error" || v.Error != "nedostupno" {
		t.Fatalf("greška: %+v", v)
	}
}
