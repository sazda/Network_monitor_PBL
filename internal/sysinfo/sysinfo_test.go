package sysinfo

import (
	"runtime"
	"testing"
	"time"
)

func TestTopByCPU(t *testing.T) {
	prev := map[procKey]procSample{
		{10, "sqlservr.exe"}: {cpu: 1000, mem: 8 << 30},
		{20, "svchost.exe"}:  {cpu: 500, mem: 50 << 20},
		{30, "staro.exe"}:    {cpu: 900},
	}
	cur := map[procKey]procSample{
		{10, "sqlservr.exe"}: {cpu: 1700, mem: 8 << 30}, // +700
		{20, "svchost.exe"}:  {cpu: 600, mem: 50 << 20}, // +100
		{40, "novi.exe"}:     {cpu: 150, mem: 1 << 20},  // pokrenut u međuvremenu
		{30, "drugi.exe"}:    {cpu: 50},                 // isti PID, drugi proces
		{50, "a.exe"}:        {mem: 9 << 20},
		{60, "b.exe"}:        {mem: 2 << 20},
	}
	got := topByCPU(prev, cur, 1000)
	want := []struct {
		name string
		pct  float64
	}{{"sqlservr.exe", 70}, {"novi.exe", 15}, {"svchost.exe", 10}, {"drugi.exe", 5}, {"a.exe", 0}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].CPUPct != w.pct {
			t.Errorf("%d: %+v, želi %s %.0f", i, got[i], w.name, w.pct)
		}
	}
}

func TestSampleProcs(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip()
	}
	var c Collector
	m, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Procs) == 0 || m.Procs[0].Name == "" || m.Procs[0].Mem == 0 && m.Procs[0].CPUPct == 0 {
		t.Fatalf("procesi: %+v", m.Procs)
	}
	t.Logf("%+v", m.Procs)
}

func TestQuick(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip()
	}
	var c Collector
	if _, ok, err := c.Quick(); err != nil || ok {
		t.Fatalf("prvo čitanje: ok=%v %v", ok, err)
	}
	time.Sleep(200 * time.Millisecond)
	l, ok, err := c.Quick()
	if err != nil || !ok || l.CPUPct < 0 || l.CPUPct > 100 || l.MemTotal == 0 || l.MemUsed == 0 {
		t.Fatalf("drugo čitanje: %+v ok=%v %v", l, ok, err)
	}
}
