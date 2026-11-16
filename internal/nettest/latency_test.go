package nettest

import (
	"testing"

	"github.com/sazda/network-monitor/internal/protocol"
)

func TestFillLatencyStats(t *testing.T) {
	r := protocol.LatencyResult{Sent: 5, Received: 4, Samples: []float64{10, 12, 11, 15}}
	FillLatencyStats(&r)
	if r.MinMS != 10 || r.MaxMS != 15 || r.AvgMS != 12 || r.LossPct != 20 {
		t.Fatalf("pogrešna statistika: %+v", r)
	}
	// |12-10| + |11-12| + |15-11| = 7, / 3
	if r.JitterMS != 2.333 {
		t.Fatalf("pogrešan jitter: %v", r.JitterMS)
	}
}

func TestFillLatencyStatsAllLost(t *testing.T) {
	r := protocol.LatencyResult{Sent: 3}
	FillLatencyStats(&r)
	if r.LossPct != 100 || r.AvgMS != 0 {
		t.Fatalf("pogrešna statistika: %+v", r)
	}
}
