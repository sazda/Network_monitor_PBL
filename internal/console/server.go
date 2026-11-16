// Package console implementira centralnu konzolu: web interfejs i REST API za
// administratora, WebSocket kanal ka agentima i HTTP krajnje tačke za merenje
// propusnosti.
package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"

	"github.com/sazda/network-monitor/internal/nettest"
	"github.com/sazda/network-monitor/internal/protocol"
)

//go:embed web
var webFS embed.FS

// Version konzole; postavlja se pri build-u (-ldflags "-X ...console.Version=...").
var Version = "dev"

// noCache traži od pretraživača da stranice konzole uvek preuzme ponovo, kako
// bi posle nadogradnje odmah dobio novi app.js umesto starog iz keša.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// Config je konfiguracija konzole.
type Config struct {
	AgentToken string // tajni token koji agenti moraju da pošalju
	AdminUser  string
	AdminPass  string // lozinka u čistom tekstu, ili
	AdminHash  string // bcrypt hash lozinke (preporučeno; vidi "console hash-password")
	Users      []User // dodatni korisnici (administratori i operateri)
	Locations  []Location
	Notify     *NotifyConfig // obaveštenja kada računar nije na vezi
	SNMP       *SNMPConfig   // čitanje mrežne opreme preko SNMP-a
	AgentMSI   string        // opciono: putanja do NetworkMonitorAgent.msi za instalaciju iz konzole
	// SyslogAddr: prijem syslog-a switch-eva (ko je pokrenuo restart);
	// prazno = ":5514", „-” = isključeno.
	SyslogAddr string
	DataDir    string
	Logger     *log.Logger

	// Opciono: fajl u koji se snimaju izmene iz web interfejsa (lozinka,
	// sertifikat) i skladište sertifikata koje se menja bez restarta.
	File  *FileConfig
	Certs *CertStore
}

// Server je HTTP server konzole.
type Server struct {
	// Održavanje switch-eva (SSH): zapamćeni ključevi i izvršilac (u
	// testovima zamenljiv).
	known     *knownHosts
	sshRun    func(ctx context.Context, addr, user, pass string, hk ssh.HostKeyCallback, cmd string) (string, error)
	ad        *adVerifier // provera domenskog naloga pre prijave na switch-eve
	maintJobs maintJobs

	cfg      Config
	hub      *Hub
	store    *Store
	log      *log.Logger
	upgrader websocket.Upgrader
	speedSem chan struct{}
	limiter  *loginLimiter

	// Keš uspešnih bcrypt provera (SHA-256 lozinke), jer web interfejs šalje
	// Basic autentifikaciju uz svaki zahtev, a bcrypt je namerno spor.
	passMu    sync.Mutex
	passCache map[[32]byte]bool
	setupMu   sync.Mutex
	sessions  *sessionStore
	deploy    *deployManager
	users     []User // čuva passMu

	locMu     sync.RWMutex
	locations []Location
	locator   locator

	notify *notifier
	srv    *serverMonitor
	// fetchQueries čita SMS upite iz sanduča (menja se u testovima).
	fetchQueries func(ctx context.Context, cfg NotifyConfig, now time.Time) ([]smsQuery, error)
	notifyMu     sync.Mutex // izmene podešavanja obaveštenja
	live         *liveTracker
	locks        agentLocks
	net          *netMonitor // mrežna oprema (switch-evi, ruteri...)
	netMu        sync.Mutex  // izmene spiska uređaja i SNMP podešavanja
	// icmpPing šalje ICMP ping sa konzole (u testovima se zamenjuje).
	pings    sync.WaitGroup // pingovi sa konzole u toku (za uredno gašenje)
	icmpPing func(ctx context.Context, host string, count int, interval, timeout time.Duration, onProbe nettest.ProbeFunc) (protocol.LatencyResult, error)
}

// New kreira server konzole.
func New(cfg Config) (*Server, error) {
	if len(cfg.AgentToken) < 16 {
		return nil, errors.New("token za agente mora imati bar 16 karaktera")
	}
	if cfg.AdminUser == "" {
		return nil, errors.New("administratorsko ime je obavezno")
	}
	// Bez lozinke konzola radi u „režimu podešavanja”: web interfejs je
	// dostupan samo sa samog servera i traži da se postavi lozinka.
	if cfg.AdminHash != "" {
		if _, err := bcrypt.Cost([]byte(cfg.AdminHash)); err != nil {
			return nil, fmt.Errorf("neispravan hash lozinke: %w", err)
		}
	} else if cfg.AdminPass != "" && len(cfg.AdminPass) < 8 {
		return nil, errors.New("administratorska lozinka mora imati bar 8 karaktera")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	store, err := OpenStore(cfg.DataDir, 5000)
	if err != nil {
		return nil, err
	}
	hub := NewHub(cfg.Logger)
	wasOnline := hub.restore(store.LoadAgents())
	hub.onSave = store.SaveAgents
	users, err := validateUsers(cfg.AdminUser, cfg.Users)
	if err != nil {
		return nil, err
	}
	loc, err := newLocator(cfg.Locations)
	if err != nil {
		return nil, fmt.Errorf("lokacije: %w", err)
	}
	s := &Server{
		users:     users,
		sessions:  newSessionStore(),
		deploy:    newDeployManager(defaultDeployRunner(cfg.DataDir)),
		known:     newKnownHosts(cfg.DataDir),
		ad:        newADVerifier(),
		locations: cfg.Locations,
		locator:   loc,
		cfg:       cfg,
		hub:       hub,
		store:     store,
		log:       cfg.Logger,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Agenti nisu pretraživači i ne šalju Origin; konekcije iz
			// pretraživača (sa Origin zaglavljem) odbijamo.
			CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" },
		},
		speedSem: make(chan struct{}, 64),
		limiter:  newLoginLimiter(),
		live:     newLiveTracker(),
		icmpPing: nettest.MeasureICMPProbe,
	}
	var nc NotifyConfig
	if cfg.Notify != nil {
		nc = *cfg.Notify
	}
	s.notify = newNotifier(nc, cfg.Logger, s.describeAgent)
	var pcs, srvs []string
	for _, id := range wasOnline {
		if a, ok := hub.Agent(id); ok && a.IsServer() {
			srvs = append(srvs, id)
		} else {
			pcs = append(pcs, id)
		}
	}
	s.notify.startupOffline(pcs)
	hub.onStatus = func(a AgentInfo) {
		if a.IsServer() {
			s.srv.changed(a)
			return
		}
		s.notify.agentChanged(a)
	}
	go s.notify.run(15 * time.Second)
	go s.notify.smsCheckLoop(30 * time.Second)
	s.fetchQueries = fetchQueries
	go s.queryLoop(queryEvery)
	s.net = newNetMonitor(cfg.DataDir, cfg.SNMP, cfg.Logger, s.notify, s.ipLocator())
	s.net.macHosts = s.agentMACs
	s.net.syslogAddr = cfg.SyslogAddr
	if s.net.syslogAddr == "" {
		s.net.syslogAddr = defaultSyslogAddr
	}
	s.net.start()
	s.srv = newServerMonitor(cfg.DataDir, hub, s.notify, cfg.Logger, func() *tsdb { return s.net.hist })
	s.srv.startup(srvs)
	hub.onMetrics = s.srv.metricsIn
	hub.onLive = s.srv.liveIn
	go s.srv.run()
	return s, nil
}

// describeAgent vraća naziv računara i lokaciju agenta (za obaveštenja).
func (s *Server) describeAgent(id string) (string, string) {
	a, ok := s.hub.Agent(id)
	if !ok {
		return "", ""
	}
	_, loc := s.currentLocations()
	return a.Hostname, loc.locate(a)
}

// Hub vraća hub (koristi se u testovima).
func (s *Server) Hub() *Hub { return s.hub }

// Handler vraća kompletan HTTP handler konzole.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Krajnje tačke za agente (autentifikacija tokenom).
	mux.Handle("GET /agent/ws", s.agentAuth(http.HandlerFunc(s.handleAgentWS)))
	mux.Handle("GET "+nettest.SpeedDownloadPath, s.agentAuth(http.HandlerFunc(s.handleSpeedDownload)))
	mux.Handle("POST "+nettest.SpeedUploadPath, s.agentAuth(http.HandlerFunc(s.handleSpeedUpload)))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	// Administratorski deo (HTTP Basic autentifikacija).
	sub, _ := fs.Sub(webFS, "web")
	// Operater: pregled i merenja. Administrator: sve.
	op := func(h http.HandlerFunc) http.Handler { return s.auth(RoleOperator, h) }
	admin := func(h http.HandlerFunc) http.Handler { return s.auth(RoleAdmin, h) }
	mux.Handle("GET /", s.auth(RoleOperator, noCache(http.FileServerFS(sub))))
	mux.Handle("GET /api/me", op(s.handleMe))
	mux.Handle("GET /api/agents", op(s.handleAgents))
	mux.Handle("POST /api/agents/{id}/latency", op(s.handleLatency))
	mux.Handle("POST /api/agents/{id}/speedtest", op(s.handleSpeedTest))
	mux.Handle("POST /api/lan-test", op(s.handleLANTest))
	mux.Handle("GET /api/results", op(s.handleResults))
	mux.Handle("GET /api/live", op(s.handleLive))
	mux.Handle("POST /api/live/{id}/stop", op(s.handleStopLive))
	mux.Handle("POST /api/agents/{id}/ping", op(s.handlePing))
	mux.Handle("GET /api/results/{id}/report.csv", op(s.handleReportCSV))
	mux.Handle("GET /api/results/{id}/report.json", op(s.handleReportJSON))
	mux.Handle("GET /api/results/{id}/report.html", op(s.handleReportHTML))
	mux.Handle("GET /api/locations", op(s.handleGetLocations))
	mux.Handle("GET /api/servers", op(s.handleServers))
	mux.Handle("GET /api/servers/events", op(s.handleServerEvents))
	mux.Handle("GET /api/servers/{id}/history", op(s.handleServerHistory))
	mux.Handle("DELETE /api/servers/{id}", admin(s.handleServerDelete))
	mux.Handle("GET /api/netdev", op(s.handleNetdevList))
	mux.Handle("GET /api/netdev/events", op(s.handleNetdevEvents))
	mux.Handle("GET /api/netdev/{id}", op(s.handleNetdevGet))
	mux.Handle("GET /api/netdev/{id}/history", op(s.handleNetdevHistory))
	mux.Handle("POST /api/netdev/{id}/ping", op(s.handleNetdevPing))
	mux.Handle("GET /api/netdev/{id}/live", op(s.handleNetdevLive))
	mux.Handle("GET /api/netdev/{id}/hosts", op(s.handleNetdevHosts))
	mux.Handle("POST /api/netdev/{id}/uplink", admin(s.handleNetdevUplink))
	mux.Handle("POST /api/netdev", admin(s.handleNetdevSave))
	mux.Handle("POST /api/netdev/import", admin(s.handleNetdevImport))
	mux.Handle("POST /api/netdev/{id}", admin(s.handleNetdevSave))
	mux.Handle("DELETE /api/netdev/{id}", admin(s.handleNetdevDelete))
	mux.Handle("POST /api/netdev/{id}/snmp-test", admin(s.handleSNMPTest))
	mux.Handle("GET /api/snmp", admin(s.handleGetSNMP))
	mux.Handle("POST /api/snmp", admin(s.handleSetSNMP))
	mux.Handle("POST /api/deploy", admin(s.handleDeploy))
	mux.Handle("GET /api/mpls", op(s.handleMPLS))
	mux.Handle("GET /api/mpls/live", op(s.handleMPLSLive))
	mux.Handle("POST /api/maint/{id}/{action}", admin(s.handleMaint))
	mux.Handle("DELETE /api/maint/{id}/hostkey", admin(s.handleMaintForgetKey))
	mux.Handle("POST /api/maintjobs", admin(s.handleMaintJobStart))
	mux.Handle("GET /api/maintjobs/{job}", admin(s.handleMaintJobStatus))
	mux.Handle("GET /api/maintjobs/{job}/zip", admin(s.handleMaintJobZip))
	mux.Handle("GET /api/deploy/{id}", admin(s.handleDeployStatus))
	mux.Handle("POST /api/locations", admin(s.handleSetLocations))
	mux.Handle("POST /api/settings/password", op(s.handleChangePassword)) // svako menja svoju
	mux.Handle("DELETE /api/agents/{id}", admin(s.handleForget))
	mux.Handle("POST /api/agents/{id}/watch", admin(s.handleWatch))
	mux.Handle("GET /api/notify", admin(s.handleGetNotify))
	mux.Handle("POST /api/notify", admin(s.handleSetNotify))
	mux.Handle("POST /api/notify/test", admin(s.handleTestNotify))
	mux.Handle("GET /api/settings", admin(s.handleSettings))
	mux.Handle("POST /api/settings/certificate", admin(s.handleUploadCertificate))
	mux.Handle("GET /api/users", admin(s.handleListUsers))
	mux.Handle("POST /api/users", admin(s.handleCreateUser))
	mux.Handle("POST /api/users/{name}", admin(s.handleUpdateUser))
	mux.Handle("DELETE /api/users/{name}", admin(s.handleDeleteUser))
	// U režimu podešavanja auth sam obrađuje ovu rutu.
	mux.Handle("POST /api/setup", s.auth(RoleAdmin, http.NotFoundHandler()))

	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// ---------- autentifikacija ----------

func (s *Server) agentAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.Header.Get(nettest.AgentTokenHeader)
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AgentToken)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// auth proverava prijavu (HTTP Basic) i ulogu korisnika. minRole je
// RoleOperator (svaki prijavljen korisnik) ili RoleAdmin.
func (s *Server) auth(minRole string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !s.limiter.allow(ip) {
			http.Error(w, "previše neuspešnih pokušaja prijave, pokušajte kasnije", http.StatusTooManyRequests)
			return
		}
		if s.needsSetup() {
			s.serveSetup(w, r)
			return
		}
		// Pretraživač: kolačić sesije. Skripte/REST API: HTTP Basic.
		id, valid := s.sessionIdentity(r)
		if !valid && basicAllowed(r) {
			if u, p, ok := r.BasicAuth(); ok {
				if id, valid = s.authenticate(u, p); !valid {
					s.limiter.fail(ip)
					s.log.Printf("neuspešna prijava (%q) sa %s", u, ip)
				}
			}
		}
		if !valid {
			s.unauthorized(w, r)
			return
		}
		if minRole == RoleAdmin && id.Role != RoleAdmin {
			writeErr(w, http.StatusForbidden, errors.New("ova akcija je dozvoljena samo administratorima"))
			return
		}
		// Zaštita od CSRF-a: izmene stanja prihvatamo samo kao JSON, što
		// pretraživač ne može da pošalje sa tuđeg sajta bez CORS dozvole.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodDelete {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "očekuje se application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		if r.Method == http.MethodDelete && r.Header.Get("X-Requested-With") == "" {
			http.Error(w, "nedostaje X-Requested-With", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, id)))
	})
}

// basicAllowed: HTTP Basic prijava važi samo za skripte i REST API. Zahtevi
// iz pretraživača (prepoznaju se po Sec-Fetch-* zaglavljima, koja šalju svi
// savremeni pretraživači, a skripte ne) koriste isključivo sesiju; inače bi
// pretraživač koji je zapamtio Basic prijavu iz ranije verzije konzole posle
// odjave odmah ponovo bio prijavljen.
func basicAllowed(r *http.Request) bool {
	return isAPIPath(r.URL.Path) &&
		r.Header.Get("Sec-Fetch-Site") == "" &&
		r.Header.Get("Sec-Fetch-Mode") == ""
}

// credentials vraća ime, hash i lozinku glavnog administratora (mogu se
// promeniti iz web interfejsa).
func (s *Server) credentials() (user, hash, pass string) {
	s.passMu.Lock()
	defer s.passMu.Unlock()
	return s.cfg.AdminUser, s.cfg.AdminHash, s.cfg.AdminPass
}

func (s *Server) needsSetup() bool {
	_, hash, pass := s.credentials()
	return hash == "" && pass == ""
}

// setCredentials postavlja novo ime i hash lozinke glavnog administratora.
func (s *Server) setCredentials(user, hash string) {
	s.passMu.Lock()
	s.cfg.AdminUser, s.cfg.AdminHash, s.cfg.AdminPass = user, hash, ""
	s.passCache = nil
	s.passMu.Unlock()
}

// HashPassword vraća bcrypt hash lozinke za upis u konfiguraciju.
func HashPassword(p string) (string, error) {
	if len(p) < 8 {
		return "", errors.New("lozinka mora imati bar 8 karaktera")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), 12)
	return string(h), err
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loginLimiter blokira IP adresu posle 10 neuspešnih prijava u 15 minuta.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string][]time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string][]time.Time{}} }

func (l *loginLimiter) prune(ip string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.fails[ip] {
		if now.Sub(t) < 15*time.Minute {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		delete(l.fails, ip)
	} else {
		l.fails[ip] = keep
	}
	return keep
}

func (l *loginLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(ip, time.Now())) < 10
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[ip] = append(l.prune(ip, time.Now()), time.Now())
}

// ---------- agent krajnje tačke ----------

func (s *Server) handleAgentWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.hub.Serve(conn, r.RemoteAddr)
}

func (s *Server) acquireSpeed(w http.ResponseWriter) bool {
	select {
	case s.speedSem <- struct{}{}:
		return true
	default:
		http.Error(w, "server je zauzet drugim testovima", http.StatusServiceUnavailable)
		return false
	}
}

func (s *Server) handleSpeedDownload(w http.ResponseWriter, r *http.Request) {
	if !s.acquireSpeed(w) {
		return
	}
	defer func() { <-s.speedSem }()
	secs, _ := strconv.Atoi(r.URL.Query().Get("seconds"))
	secs = protocol.Clamp(secs, protocol.DefaultSeconds, protocol.MaxSeconds+5)
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(secs)*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	io.Copy(w, &nettest.PayloadReader{Ctx: ctx, Counter: s.live.counter(remoteIP(r.RemoteAddr), "download")})
}

func (s *Server) handleSpeedUpload(w http.ResponseWriter, r *http.Request) {
	if !s.acquireSpeed(w) {
		return
	}
	defer func() { <-s.speedSem }()
	var dst io.Writer = io.Discard
	if c := s.live.counter(remoteIP(r.RemoteAddr), "upload"); c != nil {
		dst = nettest.CountingWriter{N: c}
	}
	n, _ := io.Copy(dst, http.MaxBytesReader(w, r.Body, 4<<30))
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"bytes":%d}`, n)
}

// ---------- administratorski API ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("neispravan JSON: %w", err)
	}
	return nil
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	agents := slices.DeleteFunc(s.hub.Agents(), AgentInfo.IsServer) // serveri su na stranici „Serveri”
	_, loc := s.currentLocations()
	for i := range agents {
		agents[i].Location = loc.locate(agents[i])
		agents[i].Watched = s.notify.isWatched(agents[i].AgentID)
		agents[i].markRelease()
	}
	writeJSON(w, http.StatusOK, agents)
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.hub.Forget(id); err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	s.notify.forget(id)
	if slices.Contains(s.notify.config().Watch, id) {
		if err := s.setWatch(id, false); err != nil {
			s.log.Printf("uklanjanje agenta iz obaveštenja: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResults(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	list, total := s.store.Query(ResultQuery{
		AgentID: r.URL.Query().Get("agent"),
		Search:  r.URL.Query().Get("q"),
		Offset:  max(offset, 0),
		Limit:   limit,
		HostName: func(id string) string {
			a, _ := s.hub.Agent(id)
			return a.Hostname
		},
	})
	for i := range list {
		list[i].Data = slimLatencyData(list[i])
	}
	// Telo ostaje niz (kao ranije, zbog skripti); ukupan broj je u zaglavlju.
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, list)
}

// record snima rezultat i šalje ga klijentu.
func (s *Server) record(w http.ResponseWriter, r *http.Request, res Result, data any, err error) {
	res = s.saveResult(res, identityFrom(r).Name, data, err)
	code := http.StatusOK
	switch {
	case errors.Is(err, ErrOffline):
		code = http.StatusConflict
	case err != nil:
		code = http.StatusBadGateway
	}
	writeJSON(w, code, res)
}

// saveResult dopunjava i snima rezultat (res.ID se zadržava ako je zadat).
func (s *Server) saveResult(res Result, user string, data any, err error) Result {
	if res.ID == "" {
		res.ID = newID()
	}
	res.User = user
	res.Time = time.Now().UTC()
	if a, ok := s.hub.Agent(res.AgentID); ok {
		res.Hostname = a.Hostname
	}
	if a, ok := s.hub.Agent(res.PeerID); ok && res.PeerID != "" {
		res.PeerHost = a.Hostname
	}
	res.OK = err == nil
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Data, _ = json.Marshal(data)
	}
	if serr := s.store.Add(res); serr != nil {
		s.log.Printf("snimanje rezultata nije uspelo: %v", serr)
	}
	return res
}

func (s *Server) handleLatency(w http.ResponseWriter, r *http.Request) {
	var p protocol.LatencyParams
	if err := decodeBody(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	release := s.lockAgents(w, r, "latency", id)
	if release == nil {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	var out protocol.LatencyResult
	var err error
	if a, ok := s.hub.Agent(id); ok && a.Online {
		target := p.Target
		switch {
		case target == "":
			target = "konzola"
		case strings.EqualFold(target, "gateway"):
			target = "gateway"
			if a.Gateway != "" {
				target = "gateway " + a.Gateway
			}
		}
		lt := s.live.start(liveView{
			Kind: "latency", AgentID: id, Host: a.Hostname, User: identityFrom(r).Name,
			Target: target, Method: p.Method, Count: protocol.Clamp(p.Count, protocol.DefaultLatencyCount, protocol.MaxLatencyCount),
			Request: p,
		}, "")
		err = s.hub.CallProgress(ctx, id, protocol.CmdLatency, p, &out, func(pr protocol.Progress) { s.live.progress(lt, pr) })
		s.live.finishLatency(lt, &out, err)
	} else {
		err = ErrOffline
	}
	raw, _ := json.Marshal(p)
	s.record(w, r, Result{AgentID: id, Kind: "latency", Params: raw}, out, err)
}

func (s *Server) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	var p protocol.SpeedTestParams
	if err := decodeBody(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	release := s.lockAgents(w, r, "speedtest", id)
	if release == nil {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	var out protocol.SpeedTestResult
	var err error
	if a, ok := s.hub.Agent(id); ok && a.Online {
		lt := s.live.start(liveView{
			Kind: "speedtest", AgentID: id, Host: a.Hostname, User: identityFrom(r).Name,
			Seconds: protocol.Clamp(p.Seconds, protocol.DefaultSeconds, protocol.MaxSeconds), Direction: protocol.NormalizeDirection(p.Direction),
			Request: p,
		}, remoteIP(a.RemoteAddr))
		err = s.hub.Call(ctx, id, protocol.CmdSpeedTest, p, &out)
		if err != nil {
			s.live.finish(lt, nil, err)
		} else {
			s.live.finish(lt, &out, nil)
		}
	} else {
		err = ErrOffline
	}
	raw, _ := json.Marshal(p)
	s.record(w, r, Result{AgentID: id, Kind: "speedtest", Params: raw}, out, err)
}

// LANTestRequest je zahtev za test brzine lokalne mreže između dva agenta.
type LANTestRequest struct {
	ClientID   string `json:"client_id"`   // agent koji meri
	ServerID   string `json:"server_id"`   // agent koji otvara privremeni server
	ServerAddr string `json:"server_addr"` // opciono: IP adresa servera (inače se bira automatski)
	Port       int    `json:"port"`        // opciono: port (0 = slučajan)
	Direction  string `json:"direction"`
	Seconds    int    `json:"seconds"`
	Streams    int    `json:"streams"`
	// LatencyCount: broj merenja latencije pre testa brzine (0 = 5).
	LatencyCount int `json:"latency_count"`
}

func (s *Server) handleLANTest(w http.ResponseWriter, r *http.Request) {
	var req LANTestRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.ClientID == "" || req.ServerID == "" || req.ClientID == req.ServerID {
		writeErr(w, http.StatusBadRequest, errors.New("potrebna su dva različita agenta"))
		return
	}
	release := s.lockAgents(w, r, "lan", req.ClientID, req.ServerID)
	if release == nil {
		return
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	raw, _ := json.Marshal(req)
	res := Result{AgentID: req.ClientID, PeerID: req.ServerID, Kind: "lan", Params: raw}
	client, _ := s.hub.Agent(req.ClientID)
	server, _ := s.hub.Agent(req.ServerID)
	lt := s.live.start(liveView{
		Kind: "lan", AgentID: req.ClientID, PeerID: req.ServerID, Host: client.Hostname, PeerHost: server.Hostname, User: identityFrom(r).Name,
		Seconds: protocol.Clamp(req.Seconds, protocol.DefaultSeconds, protocol.MaxSeconds), Direction: protocol.NormalizeDirection(req.Direction),
		Request: req,
	}, "")
	out, err := s.runLANTest(ctx, req, func(p protocol.Progress) { s.live.progress(lt, p) })
	s.live.finish(lt, out, err)
	s.record(w, r, res, out, err)
}

func (s *Server) runLANTest(ctx context.Context, req LANTestRequest, onProgress func(protocol.Progress)) (*protocol.SpeedTestResult, error) {
	client, ok := s.hub.Agent(req.ClientID)
	if !ok || !client.Online {
		return nil, fmt.Errorf("agent %s: %w", req.ClientID, ErrOffline)
	}
	tokBytes := make([]byte, 24)
	rand.Read(tokBytes)
	token := hex.EncodeToString(tokBytes)
	secs := protocol.Clamp(req.Seconds, protocol.DefaultSeconds, protocol.MaxSeconds)

	var srv protocol.LANServerResult
	err := s.hub.Call(ctx, req.ServerID, protocol.CmdLANServer, protocol.LANServerParams{
		// Server mora da traje i tokom merenja latencije (~0,1–0,5 s po paketu).
		Port: req.Port, Token: token, TTLSeconds: 2*secs + 30 + protocol.Clamp(req.LatencyCount, 5, protocol.MaxLatencyCount)/2,
	}, &srv)
	if err != nil {
		return nil, fmt.Errorf("pokretanje LAN servera na agentu %s: %w", req.ServerID, err)
	}

	host := req.ServerAddr
	if host == "" {
		var clientAddrs []string
		for _, ifc := range client.Interfaces {
			clientAddrs = append(clientAddrs, ifc.Addrs...)
		}
		seenFrom := ""
		if sa, ok := s.hub.Agent(req.ServerID); ok {
			seenFrom, _, _ = net.SplitHostPort(sa.RemoteAddr)
		}
		host = pickPeerAddr(clientAddrs, srv.Addrs, seenFrom)
		if host == "" {
			return nil, errors.New("drugi agent nema privatnu IPv4 adresu; zadajte server_addr ručno")
		}
	} else if _, err := netip.ParseAddr(host); err != nil {
		return nil, fmt.Errorf("server_addr mora biti IP adresa: %w", err)
	}

	var out protocol.SpeedTestResult
	err = s.hub.CallProgress(ctx, req.ClientID, protocol.CmdLANClient, protocol.LANClientParams{
		Addr:      net.JoinHostPort(host, strconv.Itoa(srv.Port)),
		Token:     token,
		Direction: req.Direction,
		Seconds:   secs,
		Streams:   req.Streams,

		LatencyCount: req.LatencyCount,
	}, &out, onProgress)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// pickPeerAddr bira adresu servera za LAN test (adrese su u CIDR notaciji):
//  1. adresa sa koje se server povezao na konzolu (seenFrom), ako je među
//     njegovim adresama — ona je sigurno rutabilna u mreži firme, za razliku
//     od adresa virtuelnih (VMware/Hyper-V) ili VPN adaptera;
//  2. adresa u istoj podmreži kao neka adresa klijenta (IPv4 pre IPv6);
//  3. prva privatna IPv4 adresa servera (npr. kada su u različitim VLAN-ovima).
func pickPeerAddr(clientCIDRs, serverCIDRs []string, seenFrom string) string {
	own := map[netip.Addr]bool{}
	var clients []netip.Prefix
	for _, cc := range clientCIDRs {
		if cp, err := netip.ParsePrefix(cc); err == nil {
			clients = append(clients, cp)
			own[cp.Addr()] = true
		}
	}
	var servers []netip.Prefix
	for _, sc := range serverCIDRs {
		if sp, err := netip.ParsePrefix(sc); err == nil && !own[sp.Addr()] {
			servers = append(servers, sp)
		}
	}
	if seen, err := netip.ParseAddr(seenFrom); err == nil {
		for _, sp := range servers {
			if sp.Addr() == seen.Unmap() {
				return sp.Addr().String()
			}
		}
	}
	var best, fallback netip.Addr
	for _, sp := range servers {
		for _, cp := range clients {
			if cp.Masked() == sp.Masked() && (!best.IsValid() || (sp.Addr().Is4() && !best.Is4())) {
				best = sp.Addr()
			}
		}
		if !fallback.IsValid() && sp.Addr().Is4() && sp.Addr().IsPrivate() {
			fallback = sp.Addr()
		}
	}
	switch {
	case best.IsValid():
		return best.String()
	case fallback.IsValid():
		return fallback.String()
	}
	return ""
}
