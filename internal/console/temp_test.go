package console

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"
)

// tempFake glumi senzore temperature: C9200L (entity) ili C2960L (envmon).
type tempFake struct {
	mu     sync.Mutex
	envmon bool
	vals   map[int]int // °C po indeksu senzora
	gets   int
}

func (f *tempFake) Close() {}

func (f *tempFake) num(oid string, i, v int) gosnmp.SnmpPDU {
	return gosnmp.SnmpPDU{Name: oid + "." + strconv.Itoa(i), Type: gosnmp.Integer, Value: v}
}

func (f *tempFake) str(oid string, i int, s string) gosnmp.SnmpPDU {
	return gosnmp.SnmpPDU{Name: oid + "." + strconv.Itoa(i), Type: gosnmp.OctetString, Value: []byte(s)}
}

func (f *tempFake) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.envmon {
		switch oid {
		case oidEnvTempDescr:
			return []gosnmp.SnmpPDU{f.str(oid, 1008, "SW#1, Sensor#1, GREEN")}, nil
		case oidEnvTempThresh:
			return []gosnmp.SnmpPDU{f.num(oid, 1008, 68)}, nil
		}
		return nil, nil
	}
	thr := func(s, n, v int) gosnmp.SnmpPDU {
		return gosnmp.SnmpPDU{Name: oid + "." + strconv.Itoa(s) + "." + strconv.Itoa(n), Type: gosnmp.Integer, Value: v}
	}
	switch oid {
	case oidEntSensorType:
		return []gosnmp.SnmpPDU{f.num(oid, 1001, 8), f.num(oid, 1002, 8), f.num(oid, 1003, 4)}, nil
	case oidEntSensorScale:
		return []gosnmp.SnmpPDU{f.num(oid, 1001, 9), f.num(oid, 1002, 9), f.num(oid, 1003, 8)}, nil
	case oidEntSensorPrec:
		return []gosnmp.SnmpPDU{f.num(oid, 1001, 0), f.num(oid, 1002, 0)}, nil
	case oidEntThrSeverity:
		return []gosnmp.SnmpPDU{thr(1001, 1, 20), thr(1001, 2, 30), thr(1001, 3, 20), thr(1002, 1, 20)}, nil
	case oidEntThrRelation:
		return []gosnmp.SnmpPDU{thr(1001, 1, 3), thr(1001, 2, 3), thr(1001, 3, 1), thr(1002, 1, 4)}, nil
	case oidEntThrValue:
		return []gosnmp.SnmpPDU{thr(1001, 1, 63), thr(1001, 2, 75), thr(1001, 3, 0), thr(1002, 1, 70)}, nil
	}
	return nil, nil
}

func (f *tempFake) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	names := map[int]string{1001: "Switch 1 - HotSpot Temp Sensor", 1002: "Te1/1/1 Module Temperature Sensor"}
	var out []gosnmp.SnmpPDU
	for _, o := range oids {
		i := lastIndex(o)
		switch {
		case strings.HasPrefix(o, oidEntPhysName+"."):
			out = append(out, f.str(oidEntPhysName, i, names[i]))
		case strings.HasPrefix(o, oidEntSensorValue+"."), strings.HasPrefix(o, oidEnvTempValue+"."):
			if v, ok := f.vals[i]; ok {
				out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.Gauge32, Value: uint(v)})
			} else {
				out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.NoSuchInstance})
			}
		case strings.HasPrefix(o, oidEntSensorOper+"."), strings.HasPrefix(o, oidEnvTempState+"."):
			out = append(out, gosnmp.SnmpPDU{Name: o, Type: gosnmp.Integer, Value: 1})
		}
	}
	return out, nil
}

func (f *tempFake) set(i, v int) {
	f.mu.Lock()
	f.vals[i] = v
	f.mu.Unlock()
}

func TestReadTempsEntity(t *testing.T) {
	f := &tempFake{vals: map[int]int{1001: 62, 1002: 40}}
	var ti tempInfo
	now := time.Now()
	readTemps(f, &ti, now)
	if ti.err != "" || ti.src != "entity" || len(ti.sensors) != 2 {
		t.Fatalf("otkrivanje: %+v", ti)
	}
	maxC, name, state := tempSummary(ti.sensors)
	if maxC == nil || *maxC != 62 || name != "Switch 1 - HotSpot Temp Sensor" || state != "ok" {
		t.Fatalf("zbir: %v %q %s", maxC, name, state)
	}
	s := ti.sensors[0]
	if s.Warn == nil || *s.Warn != 63 || s.Crit == nil || *s.Crit != 75 || s.Port || !ti.sensors[1].Port {
		t.Fatalf("pragovi: %+v %+v", s, ti.sensors[1])
	}
	// Sledeće čitanje: samo vrednosti (jedan Get), bez ponovnog otkrivanja.
	gets := f.gets
	f.set(1001, 64)
	readTemps(f, &ti, now.Add(time.Minute))
	if f.gets != gets+1 {
		t.Fatalf("Get poziva: %d", f.gets-gets)
	}
	if _, _, st := tempSummary(ti.sensors); st != "warning" {
		t.Fatalf("iznad praga: %s", st)
	}
	f.set(1001, 76)
	readTemps(f, &ti, now.Add(2*time.Minute))
	if _, _, st := tempSummary(ti.sensors); st != "critical" {
		t.Fatalf("kritično: %s", st)
	}
	// SFP sa pragom „>=”.
	f.set(1001, 50)
	f.set(1002, 70)
	readTemps(f, &ti, now.Add(3*time.Minute))
	if maxC, _, st := tempSummary(ti.sensors); st != "warning" || *maxC != 50 {
		t.Fatalf("SFP: %v %s", *maxC, st)
	}
}

func TestReadTempsEnvmon(t *testing.T) {
	f := &tempFake{envmon: true, vals: map[int]int{1008: 41}}
	var ti tempInfo
	readTemps(f, &ti, time.Now())
	maxC, name, state := tempSummary(ti.sensors)
	if ti.src != "envmon" || maxC == nil || *maxC != 41 || name != "SW#1, Sensor#1, GREEN" || state != "ok" || *ti.sensors[0].Crit != 68 {
		t.Fatalf("envmon: %+v %v %s", ti, maxC, state)
	}
	// Switch bez senzora.
	var none tempInfo
	readTemps(&tempFake{envmon: true, vals: map[int]int{}}, &none, time.Now())
	none.sensors = nil
	if v := newTempView(&tempInfo{src: "none", at: time.Now()}, tempAlertState{}, true); v.Error == "" || v.MaxC != nil {
		t.Fatalf("bez senzora: %+v", v)
	}
}

type tempSwitch struct {
	*fakeSNMP
	t *tempFake
}

func (c tempSwitch) Get(oids []string) ([]gosnmp.SnmpPDU, error) {
	if len(oids) > 0 && (strings.HasPrefix(oids[0], ".1.3.6.1.2.1.99.") || strings.HasPrefix(oids[0], ".1.3.6.1.2.1.47.") || strings.HasPrefix(oids[0], ".1.3.6.1.4.1.9.9.")) {
		return c.t.Get(oids)
	}
	return c.fakeSNMP.Get(oids)
}

func (c tempSwitch) BulkWalkAll(oid string) ([]gosnmp.SnmpPDU, error) {
	if strings.HasPrefix(oid, ".1.3.6.1.2.1.99.") || strings.HasPrefix(oid, ".1.3.6.1.4.1.9.9.91.") || strings.HasPrefix(oid, ".1.3.6.1.4.1.9.9.13.") {
		return c.t.BulkWalkAll(oid)
	}
	return c.fakeSNMP.BulkWalkAll(oid)
}

func TestTempAlert(t *testing.T) {
	m, sent := testMonitor(t, []NetDevice{{Name: "SW-13", IP: "10.0.35.40", Type: "switch", Active: true, SNMP: true, Notify: true}}, nil)
	tf := &tempFake{vals: map[int]int{1001: 50, 1002: 40}}
	m.dialSNMP = func(context.Context, SNMPConfig, string) (snmpClient, error) {
		return tempSwitch{&fakeSNMP{uptime: 100000}, tf}, nil
	}
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)
	m.now = func() time.Time { return now }
	m.rt("SW-13").status = stUp
	cfg := SNMPConfig{Version: "v2c", Community: "x"}
	poll := func(d time.Duration) {
		now = now.Add(d)
		m.pollSNMP(cfg, m.devices[0])
	}
	poll(0)
	v, _ := m.device("SW-13")
	if v.SNMPInfo == nil || v.SNMPInfo.Temp == nil || *v.SNMPInfo.Temp.MaxC != 50 || len(v.SNMPInfo.Temp.Sensors) != 2 {
		t.Fatalf("prikaz: %+v", v.SNMPInfo)
	}
	tf.set(1001, 64)
	poll(time.Minute)
	poll(5 * time.Minute)
	if ev := m.recentEvents("", 10); strings.Contains(fmtEvents(ev), "temp_high") {
		t.Fatal("obaveštenje pre 10 minuta")
	}
	poll(5 * time.Minute)
	evs := fmtEvents(m.recentEvents("", 10))
	if !strings.Contains(evs, "temp_high SW-13: temperatura iznad praga upozorenja od 09:01 (Switch 1 - HotSpot Temp Sensor 64 °C, prag 63 °C)") {
		t.Fatalf("događaj: %s", evs)
	}
	poll(time.Minute) // bez ponovnog obaveštenja
	tf.set(1001, 55)
	poll(time.Minute)
	evs = fmtEvents(m.recentEvents("", 10))
	if !strings.Contains(evs, "temp_ok SW-13: temperatura je ponovo u granicama (55 °C), posle 12 min") || strings.Count(evs, "temp_high") != 1 {
		t.Fatalf("povratak: %s", evs)
	}
	msgs := waitSent(t, sent, 2)
	if len(msgs) != 2 || msgs[0].Title != "Network Monitor: SW-13: visoka temperatura" || !msgs[0].Down || !msgs[1].Recovery ||
		!strings.HasPrefix(msgs[0].SMS, "NetMon ") || !strings.Contains(msgs[0].SMS, "TEMP SW-13") {
		t.Fatalf("obaveštenja: %+v", msgs)
	}
	if m.hist != nil {
		pts, err := m.hist.Query("temp/SW-13", now.Add(-time.Hour), now, false)
		if err != nil || len(pts) < 5 || pts[0].V[0] != 50 {
			t.Fatalf("istorija: %v %+v", err, pts)
		}
	}
}

func fmtEvents(evs []NetEvent) string {
	var b strings.Builder
	for _, e := range evs {
		b.WriteString(e.Kind + " " + e.Text + "\n")
	}
	return b.String()
}
