package console

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Temperatura switch-eva preko SNMP-a.
//
//   - ENTITY-SENSOR-MIB (C9200L, IOS-XE): senzori tipa 8 (°C), nazivi iz
//     ENTITY-MIB, pragovi iz CISCO-ENTITY-SENSOR-MIB (isti pragovi kao u CLI-ju
//     i SolarWinds-u: „above warning threshold”).
//   - CISCO-ENVMON-MIB (stariji IOS, npr. C2960L): vrednost, prag i stanje.
//
// Spisak senzora, nazivi i pragovi čitaju se jednom na sat (ili kad ih nema),
// a vrednosti pri svakom SNMP čitanju (jedan Get).

const (
	oidEntSensorType  = ".1.3.6.1.2.1.99.1.1.1.1"
	oidEntSensorScale = ".1.3.6.1.2.1.99.1.1.1.2"
	oidEntSensorPrec  = ".1.3.6.1.2.1.99.1.1.1.3"
	oidEntSensorValue = ".1.3.6.1.2.1.99.1.1.1.4"
	oidEntSensorOper  = ".1.3.6.1.2.1.99.1.1.1.5"
	oidEntPhysName    = ".1.3.6.1.2.1.47.1.1.1.1.7"
	oidEntThrSeverity = ".1.3.6.1.4.1.9.9.91.1.2.1.1.2"
	oidEntThrRelation = ".1.3.6.1.4.1.9.9.91.1.2.1.1.3"
	oidEntThrValue    = ".1.3.6.1.4.1.9.9.91.1.2.1.1.4"
	oidEnvTempDescr   = ".1.3.6.1.4.1.9.9.13.1.3.1.2"
	oidEnvTempValue   = ".1.3.6.1.4.1.9.9.13.1.3.1.3"
	oidEnvTempThresh  = ".1.3.6.1.4.1.9.9.13.1.3.1.4"
	oidEnvTempState   = ".1.3.6.1.4.1.9.9.13.1.3.1.6"

	tempHighFor = 10 * time.Minute // iznad praga ovoliko dugo → obaveštenje
)

// tempSensor je jedan senzor temperature.
type tempSensor struct {
	Name  string   `json:"name"`
	C     *float64 `json:"c,omitempty"`    // °C; nil = senzor ne radi
	Warn  *float64 `json:"warn,omitempty"` // prag upozorenja (switch)
	Crit  *float64 `json:"crit,omitempty"` // kritičan prag (switch)
	State string   `json:"state"`          // ok, warning, critical, unknown
	Port  bool     `json:"port,omitempty"` // senzor SFP modula (ne kućišta)
	idx   int
	scale float64
	ge    bool // prag „>=” (inače „>”)
}

// tempInfo: senzori jednog switch-a (u runtime-u i u prikazu).
type tempInfo struct {
	src     string // entity, envmon, none
	sensors []tempSensor
	at      time.Time
	discAt  time.Time
	err     string
}

// tempView je temperatura u prikazu uređaja.
type tempView struct {
	MaxC    *float64     `json:"max_c,omitempty"`
	MaxName string       `json:"max_name,omitempty"`
	State   string       `json:"state"` // ok, warning, critical, unknown, none
	Sensors []tempSensor `json:"sensors,omitempty"`
	At      time.Time    `json:"at,omitzero"`
	Since   time.Time    `json:"high_since,omitzero"` // iznad praga od
	Error   string       `json:"error,omitempty"`
}

var portSensorRe = regexp.MustCompile(`(?i)transceiver|\b(te|gi|fo|twe|hu)\d+/\d|sfp`)

func lastTwo(oid, prefix string) (int, int, bool) {
	rest := strings.TrimPrefix(strings.TrimPrefix(oid, prefix), ".")
	a, b, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, 0, false
	}
	x, err1 := strconv.Atoi(a)
	y, err2 := strconv.Atoi(b)
	return x, y, err1 == nil && err2 == nil
}

func pduFloat(p gosnmp.SnmpPDU) float64 { return float64(pduInt(p)) }

// discoverTemps čita spisak senzora temperature sa nazivima i pragovima.
func discoverTemps(c snmpClient) ([]tempSensor, string, error) {
	types, err := c.BulkWalkAll(oidEntSensorType)
	if err == nil {
		var idx []int
		for _, p := range types {
			if pduInt(p) == 8 { // celsius
				idx = append(idx, lastIndex(p.Name))
			}
		}
		if len(idx) > 0 {
			return discoverEntity(c, idx)
		}
	}
	descr, err2 := c.BulkWalkAll(oidEnvTempDescr)
	if err2 != nil {
		if err != nil {
			return nil, "", err
		}
		return nil, "", err2
	}
	if len(descr) == 0 {
		return nil, "none", nil
	}
	var out []tempSensor
	th := map[int]float64{}
	if ps, err := c.BulkWalkAll(oidEnvTempThresh); err == nil {
		for _, p := range ps {
			th[lastIndex(p.Name)] = pduFloat(p)
		}
	}
	for _, p := range descr {
		i := lastIndex(p.Name)
		s := tempSensor{Name: pduString(p), idx: i, scale: 1}
		if v, ok := th[i]; ok && v > 0 {
			s.Crit = ptr(v)
		}
		out = append(out, s)
	}
	return out, "envmon", nil
}

func discoverEntity(c snmpClient, idx []int) ([]tempSensor, string, error) {
	scale, prec := map[int]int64{}, map[int]int64{}
	for oid, m := range map[string]map[int]int64{oidEntSensorScale: scale, oidEntSensorPrec: prec} {
		if ps, err := c.BulkWalkAll(oid); err == nil {
			for _, p := range ps {
				m[lastIndex(p.Name)] = pduInt(p)
			}
		}
	}
	names := map[int]string{}
	var oids []string
	for _, i := range idx {
		oids = append(oids, fmt.Sprintf("%s.%d", oidEntPhysName, i))
	}
	for part := range slices.Chunk(oids, 40) {
		if ps, err := c.Get(part); err == nil {
			for _, p := range ps {
				names[lastIndex(p.Name)] = pduString(p)
			}
		}
	}
	// Pragovi: [senzor][prag] → ozbiljnost, odnos, vrednost.
	type thr struct {
		sev, rel int64
		val      float64
		has      bool
	}
	thrs := map[[2]int]*thr{}
	get := func(k [2]int) *thr {
		if thrs[k] == nil {
			thrs[k] = &thr{}
		}
		return thrs[k]
	}
	for _, col := range []string{oidEntThrSeverity, oidEntThrRelation, oidEntThrValue} {
		ps, err := c.BulkWalkAll(col)
		if err != nil {
			continue
		}
		for _, p := range ps {
			s, n, ok := lastTwo(p.Name, col)
			if !ok {
				continue
			}
			t := get([2]int{s, n})
			switch col {
			case oidEntThrSeverity:
				t.sev = pduInt(p)
			case oidEntThrRelation:
				t.rel = pduInt(p)
			default:
				t.val, t.has = pduFloat(p), true
			}
		}
	}
	var out []tempSensor
	for _, i := range idx {
		sc := 1.0
		if v, ok := scale[i]; ok && v > 0 {
			sc = math.Pow(1000, float64(v-9))
		}
		if p := prec[i]; p > 0 && p < 10 {
			sc /= math.Pow(10, float64(p))
		}
		s := tempSensor{Name: names[i], idx: i, scale: sc}
		if s.Name == "" {
			s.Name = fmt.Sprintf("senzor %d", i)
		}
		s.Port = portSensorRe.MatchString(s.Name)
		for k, t := range thrs {
			// Samo gornji pragovi („>” i „>=”): minor = upozorenje, major = kritično.
			if k[0] != i || !t.has || (t.rel != 3 && t.rel != 4) {
				continue
			}
			v := t.val * sc
			switch t.sev {
			case 20:
				if s.Warn == nil || v < *s.Warn {
					s.Warn, s.ge = ptr(v), t.rel == 4
				}
			case 30:
				if s.Crit == nil || v < *s.Crit {
					s.Crit = ptr(v)
				}
			}
		}
		out = append(out, s)
	}
	return out, "entity", nil
}

// readTempValues čita trenutne vrednosti poznatih senzora.
func readTempValues(c snmpClient, src string, sensors []tempSensor) error {
	valOID, stOID := oidEntSensorValue, oidEntSensorOper
	if src == "envmon" {
		valOID, stOID = oidEnvTempValue, oidEnvTempState
	}
	var oids []string
	for _, s := range sensors {
		oids = append(oids, fmt.Sprintf("%s.%d", valOID, s.idx), fmt.Sprintf("%s.%d", stOID, s.idx))
	}
	vals, states := map[int]float64{}, map[int]int64{}
	for part := range slices.Chunk(oids, 40) {
		ps, err := c.Get(part)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p.Type == gosnmp.NoSuchObject || p.Type == gosnmp.NoSuchInstance || p.Value == nil {
				continue
			}
			if strings.HasPrefix(p.Name, valOID+".") {
				vals[lastIndex(p.Name)] = pduFloat(p)
			} else {
				states[lastIndex(p.Name)] = pduInt(p)
			}
		}
	}
	for i := range sensors {
		s := &sensors[i]
		v, ok := vals[s.idx]
		st, hasSt := states[s.idx]
		s.C = nil
		// entSensorOperStatus 1 = ok; envmon 5/6 = nema senzora / ne radi.
		if !ok || src == "entity" && hasSt && st != 1 || src == "envmon" && (st == 5 || st == 6) {
			s.State = "unknown"
			continue
		}
		c := math.Round(v*s.scale*10) / 10
		s.C = &c
		s.State = "ok"
		over := func(t *float64, ge bool) bool { return t != nil && (c > *t || ge && c == *t) }
		switch {
		case over(s.Crit, false) || src == "envmon" && (st == 3 || st == 4):
			s.State = "critical"
		case over(s.Warn, s.ge) || src == "envmon" && st == 2:
			s.State = "warning"
		}
	}
	return nil
}

var tempRank = map[string]int{"unknown": 0, "ok": 1, "warning": 2, "critical": 3}

// tempSummary: najviša temperatura kućišta (SFP moduli samo ako nema drugih)
// i najgore stanje svih senzora.
func tempSummary(sensors []tempSensor) (maxC *float64, maxName, state string) {
	state = "unknown"
	pick := func(port bool) {
		for _, s := range sensors {
			if s.C != nil && s.Port == port && (maxC == nil || *s.C > *maxC) {
				maxC, maxName = ptr(*s.C), s.Name
			}
		}
	}
	pick(false)
	if maxC == nil {
		pick(true)
	}
	for _, s := range sensors {
		if tempRank[s.State] > tempRank[state] {
			state = s.State
		}
	}
	return
}

// readTemps čita temperaturu switch-a; info se menja na mestu (spisak
// senzora se pamti između čitanja).
func readTemps(c snmpClient, info *tempInfo, now time.Time) {
	if info.sensors == nil && info.src != "none" || now.Sub(info.discAt) > time.Hour {
		sensors, src, err := discoverTemps(c)
		if err != nil {
			info.err = snmpErrorText(err)
			return
		}
		info.sensors, info.src, info.discAt = sensors, src, now
	}
	if info.src == "none" || len(info.sensors) == 0 {
		info.err, info.at = "", now
		return
	}
	if err := readTempValues(c, info.src, info.sensors); err != nil {
		info.err = snmpErrorText(err)
		return
	}
	info.err, info.at = "", now
}

// ---------- praćenje praga i obaveštenja ----------

// tempAlertState je stanje praćenja temperature jednog uređaja.
type tempAlertState struct {
	since   time.Time // iznad praga upozorenja od
	alerted bool
}

// checkTempLocked beleži događaj i šalje obaveštenje kada je temperatura
// iznad praga switch-a duže od tempHighFor, i kada se vrati ispod njega.
func (m *netMonitor) checkTempLocked(d NetDevice, st *devRuntime, ti *tempInfo, now time.Time) []NetEvent {
	maxC, _, state := tempSummary(ti.sensors)
	high := state == "warning" || state == "critical"
	a := &st.temp
	var evs []NetEvent
	loc := m.locationOf(d)
	worst := ""
	for _, s := range ti.sensors {
		if (s.State == "warning" || s.State == "critical") && s.C != nil {
			lim := s.Warn
			if s.State == "critical" && s.Crit != nil {
				lim = s.Crit
			}
			w := fmt.Sprintf("%s %.0f °C", s.Name, *s.C)
			if lim != nil {
				w += fmt.Sprintf(", prag %.0f °C", *lim)
			}
			worst = w
			if s.State == "critical" {
				break
			}
		}
	}
	switch {
	case high && a.since.IsZero():
		a.since = now
	case high && !a.alerted && now.Sub(a.since) >= tempHighFor:
		a.alerted = true
		lvl := "iznad praga upozorenja"
		if state == "critical" {
			lvl = "KRITIČNA"
		}
		evs = append(evs, NetEvent{Time: now, Kind: "temp_high", DeviceID: d.ID, Device: d.Name, Location: loc,
			Text: fmt.Sprintf("%s: temperatura %s od %s (%s)", d.Name, lvl, a.since.Local().Format("15:04"), worst)})
	case !high && !a.since.IsZero():
		if a.alerted {
			t := "temperatura je ponovo u granicama"
			if maxC != nil {
				t += fmt.Sprintf(" (%.0f °C)", *maxC)
			}
			evs = append(evs, NetEvent{Time: now, Kind: "temp_ok", DeviceID: d.ID, Device: d.Name, Location: loc,
				Text: fmt.Sprintf("%s: %s, posle %s", d.Name, t, fmtDuration(now.Sub(a.since))), DurationSec: int64(now.Sub(a.since).Seconds())})
		}
		a.since, a.alerted = time.Time{}, false
	}
	return evs
}

func newTempView(ti *tempInfo, a tempAlertState, withSensors bool) *tempView {
	if ti == nil || ti.at.IsZero() && ti.err == "" {
		return nil
	}
	v := &tempView{At: ti.at, Error: ti.err, State: "none"}
	if ti.src == "none" || len(ti.sensors) == 0 {
		if ti.err == "" {
			v.Error = "switch ne daje temperaturu preko SNMP-a"
		}
		return v
	}
	v.MaxC, v.MaxName, v.State = tempSummary(ti.sensors)
	if v.State == "warning" || v.State == "critical" {
		v.Since = a.since
	}
	if withSensors {
		v.Sensors = append([]tempSensor(nil), ti.sensors...)
	}
	return v
}
