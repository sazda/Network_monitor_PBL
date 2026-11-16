// Package sysinfo čita stanje računara za agenta za servere: uptime,
// zauzeće procesora i memorije, diskove i procese koji najviše opterećuju
// procesor.
package sysinfo

import (
	"slices"
	"sync"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// topProcs: koliko procesa se šalje konzoli.
const topProcs = 5

// procKey razlikuje proces i kada Windows ponovo upotrebi isti PID.
type procKey struct {
	pid  uint32
	name string
}

// procSample: ukupno procesorsko vreme procesa (iste jedinice kao cpuTimes)
// i memorija (bajtova).
type procSample struct {
	cpu, mem uint64
}

// Collector računa zauzeće procesora kao razliku dva čitanja.
type Collector struct {
	mu        sync.Mutex
	idle, all uint64
	procs     map[procKey]procSample
	have      bool
	// Brzo čitanje (Quick) ima svoje prethodno stanje, pa prosek procesora
	// u Sample ostaje za ceo period između punih čitanja.
	qidle, qall uint64
	qhave       bool
}

// Quick vraća samo zauzeće procesora (prosek od prethodnog poziva Quick) i
// memorije; bez procesa i diskova. Prvi poziv vraća ok=false za procesor.
func (c *Collector) Quick() (protocol.Live, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var l protocol.Live
	idle, all, err := cpuTimes()
	if err != nil {
		return l, false, err
	}
	ok := false
	if d := all - c.qall; c.qhave && all > c.qall {
		l.CPUPct = max(0, 100*float64(d-(idle-c.qidle))/float64(d))
		ok = true
	}
	c.qidle, c.qall, c.qhave = idle, all, true
	if l.MemTotal, l.MemUsed, err = memory(); err != nil {
		return l, false, err
	}
	return l, ok, nil
}

// Sample vraća trenutno stanje. Prvi poziv meri procesor u toku jedne sekunde.
func (c *Collector) Sample() (protocol.Metrics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var m protocol.Metrics
	if !c.have {
		idle, all, err := cpuTimes()
		if err != nil {
			return m, err
		}
		c.procs, _ = processes()
		c.idle, c.all, c.have = idle, all, true
		time.Sleep(time.Second)
	}
	idle, all, err := cpuTimes()
	if err != nil {
		return m, err
	}
	procs, perr := processes()
	if d := all - c.all; all > c.all {
		m.CPUPct = 100 * float64(d-(idle-c.idle)) / float64(d)
		if m.CPUPct < 0 {
			m.CPUPct = 0
		}
		if perr == nil {
			m.Procs = topByCPU(c.procs, procs, d)
		}
	}
	c.idle, c.all = idle, all
	if perr == nil {
		c.procs = procs
	}
	if m.UptimeSec, err = uptime(); err != nil {
		return m, err
	}
	if m.MemTotal, m.MemUsed, err = memory(); err != nil {
		return m, err
	}
	m.Disks = disks()
	return m, nil
}

// topByCPU: procesi sa najvećim zauzećem procesora između dva čitanja;
// total je ukupno vreme svih jezgara u tom periodu. Proces koga nije bilo
// u prethodnom čitanju je pokrenut u međuvremenu, pa se računa celo
// njegovo vreme.
func topByCPU(prev, cur map[procKey]procSample, total uint64) []protocol.Proc {
	if total == 0 {
		return nil
	}
	out := make([]protocol.Proc, 0, len(cur))
	for k, v := range cur {
		d := v.cpu
		if p, ok := prev[k]; ok {
			d = 0
			if v.cpu > p.cpu {
				d = v.cpu - p.cpu
			}
		}
		pct := 100 * float64(d) / float64(total)
		if pct > 100 {
			pct = 100
		}
		out = append(out, protocol.Proc{Name: k.name, PID: k.pid, CPUPct: pct, Mem: v.mem})
	}
	slices.SortFunc(out, func(a, b protocol.Proc) int {
		switch {
		case a.CPUPct != b.CPUPct:
			if a.CPUPct > b.CPUPct {
				return -1
			}
			return 1
		case a.Mem != b.Mem:
			if a.Mem > b.Mem {
				return -1
			}
			return 1
		}
		return int(a.PID) - int(b.PID)
	})
	if len(out) > topProcs {
		out = out[:topProcs]
	}
	return out
}
