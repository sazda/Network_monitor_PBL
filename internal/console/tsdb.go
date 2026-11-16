package console

import (
	"encoding/binary"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// tsdb čuva istoriju merenja mrežne opreme (odziv, saobraćaj po portu) u
// jednom bbolt fajlu, bez spoljne baze.
//
// Podaci su podeljeni po danima: svaki dan ima svoj bucket („raw-20261007”,
// „agg-20261007”), pa se stari podaci brišu brisanjem celog bucket-a. U
// bucket-u je za svaku seriju i sat (raw) odnosno dan (agg) po jedan ključ čija
// je vrednost niz zapisa: unix vreme (uint32) + vrednosti (float32).
//
//   - raw: svako merenje (odziv na 30 s, saobraćaj na 60 s), čuva se rawDays;
//   - agg: 15-minutni zbirovi (prosek, maksimum, zbir grešaka), čuva se aggDays.
type tsdb struct {
	db       *bolt.DB
	rawDays  int
	aggDays  int
	mu       sync.Mutex
	acc      map[string]*aggAcc // 15-minutni zbir u toku, po seriji
	now      func() time.Time
	lastTrim time.Time
}

const aggStep = 15 * 60 // sekundi

// Vrste serija određuju raspored vrednosti i način sabiranja.
const (
	// seriesRTT: [odziv u ms ili NaN ako nije bilo odgovora]
	// agregat: [prosečan odziv, najveći odziv, gubitak u %]
	seriesRTT = "rtt"
	// seriesIf: [ulaz b/s, izlaz b/s, ulazne greške, izlazne greške,
	// ulazni odbačeni, izlazni odbačeni] (greške i odbačeni = broj u intervalu)
	// agregat: [prosek ulaz, prosek izlaz, max ulaz, max izlaz, zbir 4 brojača]
	seriesIf = "if"
	// seriesSrv: [CPU %, RAM %]; agregat: [prosek CPU, prosek RAM, max CPU, max RAM]
	seriesSrv = "srv"
	// seriesTemp: [najviša temperatura kućišta °C]; agregat: [prosek, max]
	seriesTemp = "temp"
)

func rawWidth(kind string) int {
	if kind == seriesRTT {
		return 1
	}
	if kind == seriesSrv {
		return 2
	}
	if kind == seriesTemp {
		return 1
	}
	return 6
}

func aggWidth(kind string) int {
	if kind == seriesRTT {
		return 3
	}
	if kind == seriesSrv {
		return 4
	}
	if kind == seriesTemp {
		return 2
	}
	return 8
}

// seriesKind vraća vrstu serije iz ključa („rtt/<uređaj>”, „if/<uređaj>/<port>”).
func seriesKind(key string) string {
	k, _, _ := strings.Cut(key, "/")
	return k
}

type aggAcc struct {
	slot int64 // početak 15-minutnog intervala (unix)
	n    int
	sum  []float64
	max  []float64
	lost int // za odziv: broj merenja bez odgovora
}

func openTSDB(dir string, rawDays, aggDays int) (*tsdb, error) {
	db, err := bolt.Open(filepath.Join(dir, "netdev-history.db"), 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("istorija mrežne opreme: %w", err)
	}
	return &tsdb{db: db, rawDays: rawDays, aggDays: aggDays, acc: map[string]*aggAcc{}, now: time.Now}, nil
}

func (t *tsdb) Close() error {
	t.flush()
	return t.db.Close()
}

func dayName(prefix string, ts time.Time) []byte {
	return []byte(prefix + ts.UTC().Format("20060102"))
}

func recKey(series string, start int64) []byte {
	b := make([]byte, len(series)+1+8)
	copy(b, series)
	b[len(series)] = 0
	binary.BigEndian.PutUint64(b[len(series)+1:], uint64(start))
	return b
}

func encodeRec(ts int64, vals []float64) []byte {
	b := make([]byte, 4+4*len(vals))
	binary.BigEndian.PutUint32(b, uint32(ts))
	for i, v := range vals {
		binary.BigEndian.PutUint32(b[4+4*i:], math.Float32bits(float32(v)))
	}
	return b
}

// appendRec dopisuje zapis na vrednost ključa k u bucket-u name.
func (t *tsdb) appendRec(name []byte, k, rec []byte) error {
	return t.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(name)
		if err != nil {
			return err
		}
		old := b.Get(k)
		nv := make([]byte, len(old)+len(rec))
		copy(nv, old)
		copy(nv[len(old):], rec)
		return b.Put(k, nv)
	})
}

// Add upisuje merenje u seriju (vreme ts) i dopunjava 15-minutni zbir.
func (t *tsdb) Add(series string, ts time.Time, vals []float64) error {
	kind := seriesKind(series)
	if len(vals) != rawWidth(kind) {
		return fmt.Errorf("serija %s: očekuje se %d vrednosti", series, rawWidth(kind))
	}
	unix := ts.Unix()
	if err := t.appendRec(dayName("raw-", ts), recKey(series, unix-unix%3600), encodeRec(unix, vals)); err != nil {
		return err
	}
	t.accumulate(series, kind, unix, vals)
	t.trimMaybe()
	return nil
}

func (t *tsdb) accumulate(series, kind string, unix int64, vals []float64) {
	slot := unix - unix%aggStep
	t.mu.Lock()
	a := t.acc[series]
	var done *aggAcc
	if a != nil && a.slot != slot {
		done = a
		a = nil
	}
	if a == nil {
		a = &aggAcc{slot: slot, sum: make([]float64, len(vals)), max: make([]float64, len(vals))}
		for i := range a.max {
			a.max[i] = math.Inf(-1)
		}
		t.acc[series] = a
	}
	a.n++
	for i, v := range vals {
		if math.IsNaN(v) {
			if kind == seriesRTT {
				a.lost++
			}
			continue
		}
		a.sum[i] += v
		a.max[i] = math.Max(a.max[i], v)
	}
	t.mu.Unlock()
	if done != nil {
		t.writeAgg(series, kind, done)
	}
}

// aggValues pretvara zbir u vrednosti agregata.
func aggValues(kind string, a *aggAcc) []float64 {
	if kind == seriesRTT {
		ok := a.n - a.lost
		avg, mx := math.NaN(), math.NaN()
		if ok > 0 {
			avg, mx = a.sum[0]/float64(ok), a.max[0]
		}
		return []float64{avg, mx, 100 * float64(a.lost) / float64(a.n)}
	}
	n := float64(a.n)
	if kind == seriesTemp {
		return []float64{a.sum[0] / n, a.max[0]}
	}
	if kind == seriesSrv {
		return []float64{a.sum[0] / n, a.sum[1] / n, a.max[0], a.max[1]}
	}
	return []float64{a.sum[0] / n, a.sum[1] / n, a.max[0], a.max[1], a.sum[2], a.sum[3], a.sum[4], a.sum[5]}
}

func (t *tsdb) writeAgg(series, kind string, a *aggAcc) {
	if a.n == 0 {
		return
	}
	t.appendRec(dayName("agg-", time.Unix(a.slot, 0)), recKey(series, a.slot-a.slot%86400), encodeRec(a.slot, aggValues(kind, a)))
}

// flush upisuje zbirove u toku (pri gašenju konzole).
func (t *tsdb) flush() {
	t.mu.Lock()
	acc := t.acc
	t.acc = map[string]*aggAcc{}
	t.mu.Unlock()
	for s, a := range acc {
		t.writeAgg(s, seriesKind(s), a)
	}
}

// trimMaybe jednom na sat briše dane starije od roka čuvanja.
func (t *tsdb) trimMaybe() {
	t.mu.Lock()
	now := t.now()
	if now.Sub(t.lastTrim) < time.Hour {
		t.mu.Unlock()
		return
	}
	t.lastTrim = now
	t.mu.Unlock()
	t.trim(now)
}

func (t *tsdb) trim(now time.Time) {
	rawCut := string(dayName("raw-", now.AddDate(0, 0, -t.rawDays)))
	aggCut := string(dayName("agg-", now.AddDate(0, 0, -t.aggDays)))
	t.db.Update(func(tx *bolt.Tx) error {
		var drop [][]byte
		tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			n := string(name)
			if (strings.HasPrefix(n, "raw-") && n < rawCut) || (strings.HasPrefix(n, "agg-") && n < aggCut) {
				drop = append(drop, append([]byte(nil), name...))
			}
			return nil
		})
		for _, n := range drop {
			tx.DeleteBucket(n)
		}
		return nil
	})
}

// DeletePrefix briše sve serije koje počinju sa prefix (uklonjen uređaj).
func (t *tsdb) DeletePrefix(prefix string) {
	t.mu.Lock()
	for s := range t.acc {
		if strings.HasPrefix(s, prefix) {
			delete(t.acc, s)
		}
	}
	t.mu.Unlock()
	t.db.Update(func(tx *bolt.Tx) error {
		return tx.ForEach(func(_ []byte, b *bolt.Bucket) error {
			var keys [][]byte
			c := b.Cursor()
			for k, _ := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, _ = c.Next() {
				keys = append(keys, append([]byte(nil), k...))
			}
			for _, k := range keys {
				b.Delete(k)
			}
			return nil
		})
	})
}

// tsPoint je jedna tačka serije: vreme (unix) i vrednosti.
type tsPoint struct {
	T int64
	V []float64
}

// Query vraća tačke serije od from do to; agg=true vraća 15-minutne zbirove.
func (t *tsdb) Query(series string, from, to time.Time, agg bool) ([]tsPoint, error) {
	kind := seriesKind(series)
	prefix, width, span := "raw-", rawWidth(kind), int64(3600)
	if agg {
		prefix, width, span = "agg-", aggWidth(kind), 86400
	}
	var out []tsPoint
	f, tt := from.Unix(), to.Unix()
	err := t.db.View(func(tx *bolt.Tx) error {
		for d := from.UTC().Truncate(24 * time.Hour); !d.After(to); d = d.Add(24 * time.Hour) {
			b := tx.Bucket(dayName(prefix, d))
			if b == nil {
				continue
			}
			c := b.Cursor()
			start := f - f%span
			if day := d.Unix(); start < day {
				start = day
			}
			end := string(recKey(series, tt))
			for k, v := c.Seek(recKey(series, start)); k != nil && string(k) <= end; k, v = c.Next() {
				if !strings.HasPrefix(string(k), series+"\x00") {
					break
				}
				rec := 4 + 4*width
				for i := 0; i+rec <= len(v); i += rec {
					ts := int64(binary.BigEndian.Uint32(v[i:]))
					if ts < f || ts > tt {
						continue
					}
					vals := make([]float64, width)
					for j := range vals {
						vals[j] = float64(math.Float32frombits(binary.BigEndian.Uint32(v[i+4+4*j:])))
					}
					out = append(out, tsPoint{T: ts, V: vals})
				}
			}
		}
		return nil
	})
	if agg {
		// Dopuna: zbir u toku (još nije upisan) za poslednjih ≤15 minuta.
		t.mu.Lock()
		if a := t.acc[series]; a != nil && a.slot >= f && a.slot <= tt && a.n > 0 {
			out = append(out, tsPoint{T: a.slot, V: aggValues(kind, a)})
		}
		t.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, err
}
