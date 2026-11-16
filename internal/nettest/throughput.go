package nettest

import (
	"context"
	"crypto/rand"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// payload je nekompresibilan blok podataka koji se šalje tokom testova, kako
// eventualna kompresija na putu (VPN, proxy) ne bi lažno povećala rezultat.
var payload = func() []byte {
	b := make([]byte, 256*1024)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}()

// Payload vraća deljeni blok nasumičnih podataka (samo za čitanje).
func Payload() []byte { return payload }

// Worker prenosi podatke dok ctx ne istekne i dodaje prenete bajtove u counter.
type Worker func(ctx context.Context, counter *atomic.Int64) error

// RunTransfer pokreće `streams` paralelnih workera u trajanju `dur`.
// Prvih ~20% vremena (najviše 1s) se tretira kao zagrevanje (TCP slow start)
// i ne ulazi u rezultat.
func RunTransfer(ctx context.Context, direction string, streams int, dur time.Duration, w Worker) (*protocol.ThroughputResult, error) {
	return RunTransferProgress(ctx, direction, streams, dur, w, nil)
}

// RunTransferProgress radi isto što i RunTransfer, uz povremeno javljanje
// trenutne brzine (report se poziva na ~500 ms; može biti nil).
func RunTransferProgress(ctx context.Context, direction string, streams int, dur time.Duration, w Worker, report func(mbps float64)) (*protocol.ThroughputResult, error) {
	warmup := dur / 5
	if warmup > time.Second {
		warmup = time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, warmup+dur)
	defer cancel()

	var counter atomic.Int64
	var wg sync.WaitGroup
	errs := make(chan error, streams)
	for i := 0; i < streams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w(ctx, &counter); err != nil && ctx.Err() == nil {
				errs <- err
			}
		}()
	}

	if report != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			last, lastT := int64(0), time.Now()
			for {
				select {
				case <-stop:
					return
				case <-ctx.Done():
					return
				case now := <-t.C:
					n := counter.Load()
					if secs := now.Sub(lastT).Seconds(); secs > 0 {
						report(math.Round(float64(n-last)*8/secs/1e6*10) / 10)
					}
					last, lastT = n, now
				}
			}
		}()
	}

	var startBytes int64
	var start time.Time
	select {
	case <-time.After(warmup):
		startBytes = counter.Load()
		start = time.Now()
	case <-ctx.Done():
	}
	wg.Wait()
	end := time.Now()
	close(errs)

	if start.IsZero() {
		// Prekinuto pre kraja zagrevanja.
		if err := <-errs; err != nil {
			return nil, err
		}
		return nil, ctx.Err()
	}
	bytes := counter.Load() - startBytes
	secs := end.Sub(start).Seconds()
	if bytes == 0 {
		if err := <-errs; err != nil {
			return nil, err
		}
	}
	return &protocol.ThroughputResult{
		Direction: direction,
		Bytes:     bytes,
		Seconds:   math.Round(secs*1000) / 1000,
		Mbps:      math.Round(float64(bytes)*8/secs/1e6*100) / 100,
		Streams:   streams,
	}, nil
}

// CountingWriter odbacuje podatke i broji ih.
type CountingWriter struct{ N *atomic.Int64 }

func (c CountingWriter) Write(p []byte) (int, error) {
	c.N.Add(int64(len(p)))
	return len(p), nil
}

// PayloadReader vraća nasumične podatke dok ctx ne istekne, pa io.EOF.
type PayloadReader struct {
	Ctx     context.Context
	Counter *atomic.Int64
	off     int
}

func (r *PayloadReader) Read(p []byte) (int, error) {
	if r.Ctx.Err() != nil {
		return 0, io.EOF
	}
	n := copy(p, payload[r.off:])
	r.off = (r.off + n) % len(payload)
	if r.Counter != nil {
		r.Counter.Add(int64(n))
	}
	return n, nil
}
