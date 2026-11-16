package console

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Održavanje za više switch-eva odjednom: administrator izabere switch-eve,
// konzola (posle provere naloga u domenu) preuzima log ili konfiguraciju sa
// po maintParallel switch-eva istovremeno, a na kraju se preuzima jedan ZIP
// sa fajlom za svaki switch i izveštajem. Lozinka postoji samo u memoriji dok
// posao traje. Kada switch odbije nalog, posao se prekida (zaključavanje
// domenskog naloga).

const (
	maintParallel  = 4
	maintJobTTL    = 30 * time.Minute
	maintJobsKept  = 10
	maintMaxSwitch = 200
	maintMaxBytes  = 256 << 20 // ukupno u memoriji za jedan posao
)

type maintJobs struct {
	mu   sync.Mutex
	list map[string]*maintJob
}

type maintResult struct {
	Name  string `json:"name"`
	IP    string `json:"ip"`
	State string `json:"state"` // waiting, running, ok, error, skipped
	Error string `json:"error,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
	file  string
	data  string
}

type maintJob struct {
	mu       sync.Mutex
	ID       string        `json:"id"`
	Action   string        `json:"action"`
	Owner    string        `json:"-"`
	Account  string        `json:"-"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished,omitzero"`
	Stopped  string        `json:"stopped,omitempty"` // razlog prekida
	Results  []maintResult `json:"results"`
	bytes    int
}

type maintJobRequest struct {
	maintRequest
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

type maintJobView struct {
	ID       string        `json:"id"`
	Action   string        `json:"action"`
	Started  time.Time     `json:"started"`
	Finished time.Time     `json:"finished,omitzero"`
	Done     bool          `json:"done"`
	Stopped  string        `json:"stopped,omitempty"`
	Total    int           `json:"total"`
	OK       int           `json:"ok"`
	Failed   int           `json:"failed"`
	Skipped  int           `json:"skipped"`
	Results  []maintResult `json:"results"`
}

func (j *maintJob) view() maintJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := maintJobView{ID: j.ID, Action: j.Action, Started: j.Started, Finished: j.Finished, Done: !j.Finished.IsZero(),
		Stopped: j.Stopped, Total: len(j.Results), Results: make([]maintResult, len(j.Results))}
	for i, r := range j.Results {
		r.data = ""
		v.Results[i] = r
		switch r.State {
		case "ok":
			v.OK++
		case "error":
			v.Failed++
		case "skipped":
			v.Skipped++
		}
	}
	return v
}

func (m *maintJobs) add(j *maintJob, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.list == nil {
		m.list = map[string]*maintJob{}
	}
	for id, x := range m.list {
		x.mu.Lock()
		old := !x.Finished.IsZero() && now.Sub(x.Finished) > maintJobTTL
		x.mu.Unlock()
		if old {
			delete(m.list, id)
		}
	}
	if len(m.list) >= maintJobsKept { // najstariji završen posao
		var oldest *maintJob
		for _, x := range m.list {
			if !x.Finished.IsZero() && (oldest == nil || x.Started.Before(oldest.Started)) {
				oldest = x
			}
		}
		if oldest != nil {
			delete(m.list, oldest.ID)
		}
	}
	m.list[j.ID] = j
}

func (m *maintJobs) get(id, owner string) *maintJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	j := m.list[id]
	if j == nil || j.Owner != owner {
		return nil
	}
	return j
}

func (m *maintJobs) running(owner string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.list {
		j.mu.Lock()
		r := j.Owner == owner && j.Finished.IsZero()
		j.mu.Unlock()
		if r {
			return true
		}
	}
	return false
}

// handleMaintJobStart: POST /api/maintjobs – pokreće preuzimanje sa više
// switch-eva (posle provere naloga u domenu).
func (s *Server) handleMaintJobStart(w http.ResponseWriter, r *http.Request) {
	var req maintJobRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	act, ok := maintActions[req.Action]
	if !ok {
		writeErr(w, http.StatusBadRequest, errors.New("nepoznata radnja"))
		return
	}
	req.User = strings.TrimSpace(req.User)
	if req.User == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, errors.New("upišite nalog i lozinku"))
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("izaberite bar jedan switch"))
		return
	}
	if len(req.IDs) > maintMaxSwitch {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("najviše %d switch-eva odjednom", maintMaxSwitch))
		return
	}
	me := identityFrom(r)
	if s.maintJobs.running(me.Name) {
		writeErr(w, http.StatusConflict, errors.New("već je pokrenuto preuzimanje – sačekajte da se završi"))
		return
	}
	type target struct{ name, ip string }
	var targets []target
	seen := map[string]bool{}
	for _, id := range req.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		v, ok := s.net.device(id)
		if !ok {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("uređaj %q nije pronađen", id))
			return
		}
		if v.SNMPInfo == nil || !maintModelOK(v.SNMPInfo.Model) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("%s: održavanje je dostupno samo za Cisco C9200L i C2960L", v.Name))
			return
		}
		targets = append(targets, target{v.Name, v.IP})
	}
	if !s.checkDomainAccount(w, me, req.maintRequest) {
		return
	}
	idb := make([]byte, 8)
	rand.Read(idb)
	j := &maintJob{ID: hex.EncodeToString(idb), Action: req.Action, Owner: me.Name, Account: req.User, Started: time.Now()}
	stamp := j.Started.Format("20060102-1504")
	used := map[string]bool{}
	for _, t := range targets {
		f := fmt.Sprintf("%s-%s-%s.txt", safeFileName(t.name), act.file, stamp)
		if used[f] {
			f = fmt.Sprintf("%s-%s-%s-%s.txt", safeFileName(t.name), safeFileName(t.ip), act.file, stamp)
		}
		used[f] = true
		j.Results = append(j.Results, maintResult{Name: t.name, IP: t.ip, State: "waiting", file: f})
	}
	s.maintJobs.add(j, j.Started)
	s.log.Printf("održavanje: %s (nalog na switch-u %s) pokrenuo preuzimanje – %s sa %d switch-eva", me.Name, req.User, act.label, len(targets))
	go s.runMaintJob(j, act.cmd, act.label, req.User, req.Password)
	writeJSON(w, http.StatusAccepted, j.view())
}

func (s *Server) runMaintJob(j *maintJob, cmd, label, user, pass string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	run := s.sshRunner()
	sem := make(chan struct{}, maintParallel)
	var wg sync.WaitGroup
	for i := range j.Results {
		sem <- struct{}{}
		j.mu.Lock()
		stopped := j.Stopped != ""
		if !stopped {
			j.Results[i].State = "running"
		}
		name, ip := j.Results[i].Name, j.Results[i].IP
		j.mu.Unlock()
		if stopped {
			<-sem
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			out, err := run(ctx, net.JoinHostPort(ip, "22"), user, pass, s.known.callback(ip), cmd)
			j.mu.Lock()
			defer j.mu.Unlock()
			r := &j.Results[i]
			switch {
			case err == nil && j.bytes+len(out) > maintMaxBytes:
				r.State, r.Error = "error", "preuzeto je previše podataka u ovom poslu – preuzmite ovaj switch posebno"
			case err == nil:
				r.State, r.Bytes, r.data = "ok", len(out), out
				j.bytes += len(out)
			default:
				r.State, r.Error = "error", err.Error()
				if errors.Is(err, errHostKeyChanged) {
					r.Error += " (ako je switch zamenjen, pokrenite radnju samo za njega i zaboravite stari ključ)"
				}
				if errors.Is(err, errSwitchAuth) && j.Stopped == "" {
					j.Stopped = fmt.Sprintf("%s je odbio nalog %s – preuzimanje je prekinuto da se domenski nalog ne bi zaključao", name, user)
				}
			}
			s.log.Printf("održavanje: %s – %s sa %s (%s): %s", j.Owner, label, name, ip, logResult(r))
		}()
	}
	wg.Wait()
	j.mu.Lock()
	for i := range j.Results {
		if j.Results[i].State == "waiting" {
			j.Results[i].State, j.Results[i].Error = "skipped", "preskočeno – preuzimanje je prekinuto"
		}
	}
	j.Finished = time.Now()
	ok := 0
	for _, r := range j.Results {
		if r.State == "ok" {
			ok++
		}
	}
	j.mu.Unlock()
	s.log.Printf("održavanje: %s – preuzimanje (%s) završeno: %d od %d switch-eva", j.Owner, label, ok, len(j.Results))
}

func logResult(r *maintResult) string {
	if r.State == "ok" {
		return fmt.Sprintf("preuzeto %d bajtova", r.Bytes)
	}
	return "nije uspelo – " + r.Error
}

func (s *Server) handleMaintJobStatus(w http.ResponseWriter, r *http.Request) {
	j := s.maintJobs.get(r.PathValue("job"), identityFrom(r).Name)
	if j == nil {
		writeErr(w, http.StatusNotFound, errors.New("preuzimanje nije pronađeno (možda je isteklo)"))
		return
	}
	writeJSON(w, http.StatusOK, j.view())
}

// handleMaintJobZip: ZIP sa fajlom za svaki uspešno preuzet switch i
// izveštajem (izvestaj.txt).
func (s *Server) handleMaintJobZip(w http.ResponseWriter, r *http.Request) {
	j := s.maintJobs.get(r.PathValue("job"), identityFrom(r).Name)
	if j == nil {
		writeErr(w, http.StatusNotFound, errors.New("preuzimanje nije pronađeno (možda je isteklo)"))
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Finished.IsZero() {
		writeErr(w, http.StatusConflict, errors.New("preuzimanje još traje"))
		return
	}
	act := maintActions[j.Action]
	name := fmt.Sprintf("switch-%s-%s.zip", act.file, j.Started.Format("20060102-1504"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	defer zw.Close()
	res := append([]maintResult(nil), j.Results...)
	sort.SliceStable(res, func(a, b int) bool { return res[a].Name < res[b].Name })
	for _, x := range res {
		if x.State != "ok" {
			continue
		}
		f, err := zw.CreateHeader(&zip.FileHeader{Name: x.file, Method: zip.Deflate, Modified: j.Finished})
		if err != nil {
			return
		}
		f.Write([]byte(x.data))
	}
	f, err := zw.CreateHeader(&zip.FileHeader{Name: "izvestaj.txt", Method: zip.Deflate, Modified: j.Finished})
	if err != nil {
		return
	}
	f.Write([]byte(maintReport(j, act.label, res)))
}

func maintReport(j *maintJob, label string, res []maintResult) string {
	var b strings.Builder
	ok := 0
	for _, r := range res {
		if r.State == "ok" {
			ok++
		}
	}
	fmt.Fprintf(&b, "Network Monitor - preuzimanje: %s\r\n", label)
	fmt.Fprintf(&b, "Pokrenuo: %s (nalog na switch-evima: %s)\r\n", j.Owner, j.Account)
	fmt.Fprintf(&b, "Vreme: %s - %s\r\n", j.Started.Local().Format("02.01.2006. 15:04:05"), j.Finished.Local().Format("15:04:05"))
	fmt.Fprintf(&b, "Uspešno: %d od %d\r\n", ok, len(res))
	if j.Stopped != "" {
		fmt.Fprintf(&b, "PREKINUTO: %s\r\n", j.Stopped)
	}
	b.WriteString("\r\n")
	for _, r := range res {
		switch r.State {
		case "ok":
			fmt.Fprintf(&b, "OK       %-24s %-15s %s (%d bajtova)\r\n", r.Name, r.IP, r.file, r.Bytes)
		case "skipped":
			fmt.Fprintf(&b, "PRESKOČ. %-24s %-15s %s\r\n", r.Name, r.IP, r.Error)
		default:
			fmt.Fprintf(&b, "GREŠKA   %-24s %-15s %s\r\n", r.Name, r.IP, r.Error)
		}
	}
	return b.String()
}
