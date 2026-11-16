package console

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sazda/network-monitor/internal/protocol"
)

// Instalacija agenata iz konzole: administrator upiše nazive računara i svoj
// administratorski nalog, a konzola preko WinRM-a kopira MSI agenta i
// instalira ga sa svojom adresom i tokenom. Kredencijali se koriste samo za
// taj posao i nigde se ne čuvaju niti upisuju u log.

const (
	deployParallel = 4
	deployTimeout  = 5 * time.Minute
	deployKeepJobs = 20
	maxDeployHosts = 500
)

// deployTarget je ulaz za instalaciju na jednom računaru.
type deployTarget struct {
	Computer string `json:"computer"`
	User     string `json:"user"`
	Password string `json:"password"`
	MSI      string `json:"msi"`
	URL      string `json:"url"`
	Token    string `json:"token"`
	Upgrade  bool   `json:"upgrade"`
	// Paket i servis na ciljnom računaru (agent za računare ili za servere).
	MSIName string `json:"msi_name"`
	Service string `json:"service"`
}

// Vrste agenata za instalaciju iz konzole.
const (
	agentMSIName       = "NetworkMonitorAgent.msi"
	serverAgentMSIName = "NetworkMonitorServerAgent.msi"
)

// deployOutcome je rezultat skripte za jedan računar.
type deployOutcome struct {
	OK       bool   `json:"ok"`
	ExitCode *int   `json:"exitCode"`
	Service  string `json:"service"`
	Message  string `json:"message"`
}

// deployRunner izvršava instalaciju na jednom računaru (u testovima se menja).
type deployRunner func(ctx context.Context, t deployTarget) deployOutcome

// DeployItem je stanje instalacije na jednom računaru.
type DeployItem struct {
	Host    string `json:"host"`
	Target  string `json:"target"` // naziv koji se koristi za WinRM (npr. posle obrnutog DNS upita za IP)
	Status  string `json:"status"` // pending | running | ok | error
	Message string `json:"message,omitempty"`
}

// DeployJob je jedno pokretanje instalacije na više računara.
type DeployJob struct {
	ID       string       `json:"id"`
	Started  time.Time    `json:"started"`
	By       string       `json:"by"`
	Upgrade  bool         `json:"upgrade"`
	Finished bool         `json:"finished"`
	Items    []DeployItem `json:"items"`
}

type deployManager struct {
	mu   sync.Mutex
	jobs map[string]*DeployJob
	run  deployRunner
}

func newDeployManager(run deployRunner) *deployManager {
	return &deployManager{jobs: map[string]*DeployJob{}, run: run}
}

func (m *deployManager) snapshot(id string) (DeployJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return DeployJob{}, false
	}
	cp := *j
	cp.Items = append([]DeployItem(nil), j.Items...)
	return cp, true
}

func (m *deployManager) update(id string, i int, fn func(*DeployItem)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		fn(&j.Items[i])
	}
}

func (m *deployManager) add(j *DeployJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[j.ID] = j
	if len(m.jobs) > deployKeepJobs {
		var ids []string
		for id := range m.jobs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(a, b int) bool { return m.jobs[ids[a]].Started.Before(m.jobs[ids[b]].Started) })
		for _, id := range ids[:len(ids)-deployKeepJobs] {
			if m.jobs[id].Finished {
				delete(m.jobs, id)
			}
		}
	}
}

var hostNameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*$`)

// parseHosts čita nazive računara/IP adrese (razdvojene novim redom, zarezom,
// tačka-zarezom ili razmakom) i uklanja duplikate.
func parseHosts(list []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, item := range list {
		for _, h := range strings.FieldsFunc(item, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		}) {
			if net.ParseIP(h) == nil && (len(h) > 253 || !hostNameRe.MatchString(h)) {
				return nil, fmt.Errorf("neispravan naziv računara: %q", h)
			}
			if !seen[strings.ToLower(h)] {
				seen[strings.ToLower(h)] = true
				out = append(out, h)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("upišite bar jedan računar")
	}
	if len(out) > maxDeployHosts {
		return nil, fmt.Errorf("najviše %d računara odjednom", maxDeployHosts)
	}
	return out, nil
}

// winrmTarget pretvara IP adresu u naziv računara (obrnutim DNS upitom), jer
// WinRM sa domenskim nalogom (Kerberos) radi samo sa nazivima.
func winrmTarget(ctx context.Context, host string) string {
	if net.ParseIP(host) == nil {
		return host
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(ctx, host)
	if err != nil || len(names) == 0 {
		return host
	}
	return strings.TrimSuffix(names[0], ".")
}

// describeOutcome pretvara rezultat skripte u poruku za korisnika.
func describeOutcome(o deployOutcome, upgrade bool) (ok bool, msg string) {
	if o.ExitCode == nil {
		m := o.Message
		low := strings.ToLower(m)
		switch {
		case m == "":
			m = "nepoznata greška"
		case strings.Contains(low, "access is denied") || strings.Contains(low, "pristup je odbijen"):
			m = "pristup odbijen: nalog nije lokalni administrator na tom računaru (" + m + ")"
		case strings.Contains(low, "winrm") || strings.Contains(low, "cannot connect") || strings.Contains(low, "cannot be resolved"):
			m = "WinRM nije dostupan (računar ugašen, WinRM isključen ili blokiran firewall-om): " + m
		case strings.Contains(low, "user name or password"):
			m = "pogrešno korisničko ime ili lozinka"
		}
		return false, m
	}
	switch *o.ExitCode {
	case 0, 3010:
		verb := "instaliran"
		if upgrade {
			verb = "nadograđen"
		}
		msg = verb
		if *o.ExitCode == 3010 {
			msg += ", potreban restart"
		}
		if o.Service != "" {
			msg += " (servis: " + o.Service + ")"
		}
		return true, msg
	case 1603:
		return false, "MSI greška 1603: instalacija nije uspela (npr. agent ranije instaliran drugim putem)"
	case 1618:
		return false, "MSI greška 1618: u toku je druga instalacija, pokušajte kasnije"
	case 1638:
		return false, "MSI greška 1638: ista ili novija verzija je već instalirana"
	case 1619:
		return false, "MSI greška 1619: paket nije prenet na računar"
	default:
		return false, fmt.Sprintf("msiexec izlazni kod %d", *o.ExitCode)
	}
}

// start pokreće posao u pozadini.
func (m *deployManager) start(ctx context.Context, by string, hosts []string, base deployTarget, logf func(string, ...any)) *DeployJob {
	job := &DeployJob{ID: newID(), Started: time.Now().UTC(), By: by, Upgrade: base.Upgrade}
	for _, h := range hosts {
		job.Items = append(job.Items, DeployItem{Host: h, Status: "pending"})
	}
	m.add(job)
	go func() {
		sem := make(chan struct{}, deployParallel)
		var wg sync.WaitGroup
		for i, h := range hosts {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int, h string) {
				defer func() { <-sem; wg.Done() }()
				target := winrmTarget(ctx, h)
				m.update(job.ID, i, func(it *DeployItem) { it.Status, it.Target = "running", target })
				t := base
				t.Computer = target
				cctx, cancel := context.WithTimeout(ctx, deployTimeout)
				out := m.run(cctx, t)
				cancel()
				ok, msg := describeOutcome(out, base.Upgrade)
				status := "ok"
				if !ok {
					status = "error"
				}
				m.update(job.ID, i, func(it *DeployItem) { it.Status, it.Message = status, msg })
				logf("instalacija agenta na %s (%s): %s", h, status, msg)
			}(i, h)
		}
		wg.Wait()
		m.mu.Lock()
		job.Finished = true
		m.mu.Unlock()
	}()
	return job
}

// agentMSIPath vraća putanju do MSI-ja agenta za računare.
func (s *Server) agentMSIPath() (string, error) { return s.msiPath(agentMSIName) }

// msiPath vraća putanju do MSI paketa agenta: pored console.exe (tamo ih
// stavlja MSI konzole) ili u direktorijumu sa konfiguracijom.
func (s *Server) msiPath(name string) (string, error) {
	var candidates []string
	if s.cfg.AgentMSI != "" {
		if name == agentMSIName {
			candidates = append(candidates, s.cfg.AgentMSI)
		} else {
			candidates = append(candidates, filepath.Join(filepath.Dir(s.cfg.AgentMSI), name))
		}
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	if s.cfg.File != nil {
		candidates = append(candidates, filepath.Join(s.cfg.File.Dir(), name))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s nije pronađen pored console.exe; instalirajte novu verziju konzole (MSI konzole sadrži i pakete agenata)", name)
}

// defaultAgentURL predlaže adresu konzole za agente: prvo ime iz sertifikata
// (ako nije privremeni) i port na kom konzola sluša.
func (s *Server) defaultAgentURL() string {
	if s.cfg.Certs == nil {
		return ""
	}
	info := s.cfg.Certs.Info()
	if info == nil || info.SelfSigned || len(info.DNSNames) == 0 {
		return ""
	}
	port := "443"
	if s.cfg.File != nil {
		if _, p, err := net.SplitHostPort(s.cfg.File.Listen); err == nil && p != "" {
			port = p
		}
	}
	host := info.DNSNames[0]
	if port != "443" {
		host = net.JoinHostPort(host, port)
	}
	return "https://" + host
}

type deployRequest struct {
	Hosts      []string `json:"hosts"`
	ConsoleURL string   `json:"console_url"`
	User       string   `json:"user"`
	Password   string   `json:"password"`
	Upgrade    bool     `json:"upgrade"`
	Kind       string   `json:"kind"` // "" = agent za računare, "server" = agent za servere
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var req deployRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	hosts, err := parseHosts(req.Hosts)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.User) == "" || req.Password == "" {
		writeErr(w, http.StatusBadRequest, errors.New("upišite administratorski nalog i lozinku za ciljne računare"))
		return
	}
	if !req.Upgrade {
		u, err := url.Parse(strings.TrimSpace(req.ConsoleURL))
		if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") {
			writeErr(w, http.StatusBadRequest, errors.New("adresa konzole mora biti u obliku https://ime:port"))
			return
		}
		if h := u.Hostname(); h == "localhost" || net.ParseIP(h) != nil && net.ParseIP(h).IsLoopback() {
			writeErr(w, http.StatusBadRequest, errors.New("adresa konzole ne sme biti localhost; upišite ime servera (npr. iz sertifikata)"))
			return
		}
		req.ConsoleURL = strings.TrimSuffix(u.String(), "/")
	}
	name, svc, what := agentMSIName, "network-monitor-agent", "agenata za računare"
	if req.Kind == protocol.RoleServer {
		name, svc, what = serverAgentMSIName, "network-monitor-server-agent", "agenata za servere"
	} else if req.Kind != "" {
		writeErr(w, http.StatusBadRequest, errors.New("nepoznata vrsta agenta"))
		return
	}
	msi, err := s.msiPath(name)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	base := deployTarget{
		User: strings.TrimSpace(req.User), Password: req.Password,
		MSI: msi, URL: req.ConsoleURL, Token: s.cfg.AgentToken, Upgrade: req.Upgrade,
		MSIName: name, Service: svc,
	}
	me := identityFrom(r)
	s.log.Printf("instalacija %s pokrenuta: %d računara, nadogradnja=%v, pokrenuo %q, nalog za računare %q",
		what, len(hosts), req.Upgrade, me.Name, base.User)
	job := s.deploy.start(context.Background(), me.Name, hosts, base, s.log.Printf)
	snap, _ := s.deploy.snapshot(job.ID)
	writeJSON(w, http.StatusAccepted, snap)
}

func (s *Server) handleDeployStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.deploy.snapshot(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("posao ne postoji"))
		return
	}
	writeJSON(w, http.StatusOK, job)
}
