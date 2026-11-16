package sysinfo

import (
	"slices"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// IfRates računa brzinu mrežnih kartica između dva čitanja brojača.
type IfRates struct {
	prev map[string][2]uint64
	at   time.Time
}

// Next vraća brzine od prethodnog poziva (prvi poziv vraća nil). Kartica čiji
// se brojač smanjio (restart kartice) preskače se u tom krugu.
func (r *IfRates) Next(cur map[string][2]uint64, now time.Time) []protocol.IfRate {
	prev, at := r.prev, r.at
	r.prev, r.at = cur, now
	dt := now.Sub(at).Seconds()
	if prev == nil || dt <= 0 {
		return nil
	}
	var out []protocol.IfRate
	for mac, c := range cur {
		p, ok := prev[mac]
		if !ok || c[0] < p[0] || c[1] < p[1] {
			continue
		}
		out = append(out, protocol.IfRate{MAC: mac, Recv: float64(c[0]-p[0]) * 8 / dt, Send: float64(c[1]-p[1]) * 8 / dt})
	}
	slices.SortFunc(out, func(a, b protocol.IfRate) int {
		if a.MAC < b.MAC {
			return -1
		}
		return 1
	})
	return out
}
