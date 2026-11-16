package sysinfo

import (
	"testing"
	"time"
)

func TestIfRates(t *testing.T) {
	var r IfRates
	t0 := time.Unix(1000, 0)
	if r.Next(map[string][2]uint64{"aa": {1000, 500}}, t0) != nil {
		t.Fatal("prvo čitanje nema brzinu")
	}
	got := r.Next(map[string][2]uint64{"aa": {1000 + 125000, 500 + 12500}, "bb": {5, 5}}, t0.Add(10*time.Second))
	if len(got) != 1 || got[0].MAC != "aa" || got[0].Recv != 100000 || got[0].Send != 10000 {
		t.Fatalf("%+v", got)
	}
	// Brojač se smanjio (restart kartice): preskače se.
	if got := r.Next(map[string][2]uint64{"aa": {10, 10}, "bb": {85, 5}}, t0.Add(20*time.Second)); len(got) != 1 || got[0].MAC != "bb" || got[0].Recv != 64 {
		t.Fatalf("%+v", got)
	}
}

func TestIfCountersHere(t *testing.T) {
	c, err := IfCounters()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%v", c)
}
