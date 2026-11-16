package console

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// NotifyConfig su podešavanja obaveštenja kada računar (agent) nije na vezi.
// Obaveštenje može da ide e-mailom (npr. Outlook na telefonu), u Microsoft
// Teams (Workflows webhook) i preko ntfy servisa (ntfy aplikacija na telefonu).
type NotifyConfig struct {
	Enabled      bool     `json:"enabled"`
	AllAgents    bool     `json:"all_agents"`              // false = samo označeni računari (Watch)
	Watch        []string `json:"watch,omitempty"`         // ID-jevi označenih agenata
	DelayMinutes int      `json:"delay_minutes,omitempty"` // koliko dugo agent mora biti van veze (podrazumevano 5)
	SkipRecovery bool     `json:"skip_recovery,omitempty"` // ne javljati kada se računar vrati

	EmailHost     string `json:"email_host,omitempty"` // SMTP server (npr. interni Exchange relay)
	EmailPort     int    `json:"email_port,omitempty"` // podrazumevano 25; 465 = TLS od početka
	EmailTLS      string `json:"email_tls,omitempty"`  // "" = STARTTLS ako ga server nudi, "off" = bez TLS-a
	EmailUser     string `json:"email_user,omitempty"`
	EmailPassword string `json:"email_password,omitempty"`
	EmailFrom     string `json:"email_from,omitempty"`
	EmailTo       string `json:"email_to,omitempty"` // adrese razdvojene zarezom

	TeamsURL  string `json:"teams_url,omitempty"`  // Teams Workflows: "When a Teams webhook request is received"
	NtfyURL   string `json:"ntfy_url,omitempty"`   // npr. https://ntfy.sh/<tajna-tema> ili interni ntfy server
	NtfyToken string `json:"ntfy_token,omitempty"` // opciono (ntfy access token)

	Proxy string `json:"proxy,omitempty"` // opciono: HTTP proxy za Teams/ntfy (npr. http://proxy:8080)

	// SMS preko e-maila (samo mrežna oprema): sanduče koje čita Android
	// telefon sa aplikacijom NetMon SMS i vreme dnevne provere (SS:MM, "" = bez).
	SMSEmail     string `json:"sms_email,omitempty"`
	SMSCheckTime string `json:"sms_check_time,omitempty"`

	// SMS upiti („status <lokacija>” sa telefona): konzola čita sanduče SMS
	// telefona preko IMAP-a istim nalogom kao SMTP. IMAPHost "" = SMTP server.
	SMSQueries bool   `json:"sms_queries,omitempty"`
	IMAPHost   string `json:"imap_host,omitempty"`
	IMAPPort   int    `json:"imap_port,omitempty"`
	// SMSAliases: skraćenice lokacija za upite („GC = Glavni Centar”, jedna
	// po redu); "" = podrazumevane (defaultAliases).
	SMSAliases string `json:"sms_aliases,omitempty"`
	// SMSServerAliases: skraćenice servera za upit „server SOL”
	// („SOL = srv-monitor”, jedna po redu).
	SMSServerAliases string `json:"sms_server_aliases,omitempty"`

	// Serveri (agent za servere): obaveštenje posle N minuta bez veze
	// (podrazumevano 2) i kada je CPU/RAM iznad praga (90%) duže od N minuta (10).
	ServerDelayMinutes int `json:"server_delay_minutes,omitempty"`
	ServerCPUPct       int `json:"server_cpu_pct,omitempty"`
	ServerMemPct       int `json:"server_mem_pct,omitempty"`
	ServerHighMinutes  int `json:"server_high_minutes,omitempty"`

	// Prekidači po kanalu (nil = podešavanja iz starije verzije, vidi normalize).
	Channels *NotifyChannels `json:"channels,omitempty"`
}

func (c *NotifyConfig) delay() time.Duration {
	if c.DelayMinutes <= 0 {
		return 5 * time.Minute
	}
	return time.Duration(c.DelayMinutes) * time.Minute
}

func (c *NotifyConfig) emailEnabled() bool { return c.EmailHost != "" }

func (c *NotifyConfig) recipients() []string {
	var out []string
	for _, a := range strings.Split(c.EmailTo, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

// validate proverava podešavanja i popunjava podrazumevane vrednosti.
func (c *NotifyConfig) validate() error {
	if c.DelayMinutes < 0 || c.DelayMinutes > 24*60 {
		return errors.New("vreme čekanja mora biti između 1 i 1440 minuta")
	}
	if c.EmailTLS != "" && c.EmailTLS != "off" {
		return errors.New("neispravan TLS režim za e-mail")
	}
	if c.emailEnabled() {
		if c.EmailPort == 0 {
			c.EmailPort = 25
		}
		if c.EmailPort < 1 || c.EmailPort > 65535 {
			return errors.New("neispravan SMTP port")
		}
		if _, err := mail.ParseAddress(c.EmailFrom); err != nil {
			return fmt.Errorf("adresa pošiljaoca: %w", err)
		}
		for _, a := range c.recipients() {
			if _, err := mail.ParseAddress(a); err != nil {
				return fmt.Errorf("adresa primaoca %q: %w", a, err)
			}
		}
		if c.EmailUser != "" && c.EmailTLS == "off" {
			return errors.New("prijava na SMTP server bez TLS-a nije dozvoljena (lozinka bi išla nešifrovano)")
		}
	}
	if c.TeamsURL != "" {
		if err := checkURL(c.TeamsURL, true); err != nil {
			return fmt.Errorf("Teams adresa: %w", err)
		}
	}
	if c.NtfyURL != "" {
		if err := checkURL(c.NtfyURL, false); err != nil {
			return fmt.Errorf("ntfy adresa: %w", err)
		}
	}
	if c.Proxy != "" {
		if err := checkURL(c.Proxy, false); err != nil {
			return fmt.Errorf("proxy: %w", err)
		}
	}
	if err := c.validateSMS(); err != nil {
		return err
	}
	if err := c.validateQueries(); err != nil {
		return err
	}
	if err := c.validateServers(); err != nil {
		return err
	}
	c.normalize()
	return c.validateChannels()
}

func checkURL(s string, httpsOnly bool) error {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && (httpsOnly || u.Scheme != "http")) {
		if httpsOnly {
			return errors.New("očekuje se https:// adresa")
		}
		return errors.New("očekuje se http:// ili https:// adresa")
	}
	return nil
}

// notifyMessage je jedno (grupisano) obaveštenje.
type notifyMessage struct {
	Title    string
	Body     string
	Down     bool // ima računara koji nisu na vezi (viši prioritet)
	Recovery bool

	Scope   string // scopeAgents ili scopeNet: bira kanale
	SMS     string // tekst za SMS ("" = pravi se iz naslova)
	Channel string // samo jedan kanal, bez obzira na prekidače (proba, dnevna provera)
	// HTML i slike (cid:) samo za e-mail; ostali kanali koriste Body.
	HTML   string
	Images []mailImage
}

// mailImage je slika ugrađena u HTML e-mail (src="cid:<CID>").
type mailImage struct {
	CID  string
	Name string
	Data []byte
}

// notifyStatus je stanje poslednjeg slanja (prikazuje se u podešavanjima).
type notifyStatus struct {
	LastSent    time.Time `json:"last_sent,omitzero"`
	LastTitle   string    `json:"last_title,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`

	Channels map[string]*channelStatus `json:"channels,omitempty"`

	QueryAt      time.Time `json:"query_at,omitzero"` // poslednje uspešno čitanje SMS upita
	QueryError   string    `json:"query_error,omitempty"`
	QueryErrorAt time.Time `json:"query_error_at,omitzero"`
}

// channelStatus je stanje poslednjeg slanja na jedan kanal.
type channelStatus struct {
	LastSent    time.Time `json:"last_sent,omitzero"`
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
}

// channelResult je ishod slanja na jedan kanal.
type channelResult struct {
	Key     string `json:"key"`
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

type downState struct {
	since   time.Time
	alerted bool
}

type upEvent struct {
	id      string
	downFor time.Duration
}

// notifier prati prekide veze agenata i šalje grupisana obaveštenja.
type notifier struct {
	log *log.Logger

	mu      sync.Mutex
	cfg     NotifyConfig
	down    map[string]*downState
	ups     []upEvent
	status  notifyStatus
	sending sync.Mutex // jedno slanje u isto vreme

	// describe vraća naziv računara i lokaciju za ID agenta.
	describe func(id string) (host, loc string)
	// send šalje poruku na sve podešene kanale (menja se u testovima).
	send func(ctx context.Context, cfg NotifyConfig, m notifyMessage) []channelResult
	// sendSMS šalje SMS tekst na sanduče telefona (menja se u testovima).
	sendSMS func(ctx context.Context, cfg NotifyConfig, text string) error
	now     func() time.Time
}

func newNotifier(cfg NotifyConfig, logger *log.Logger, describe func(string) (string, string)) *notifier {
	cfg.normalize()
	return &notifier{
		log:      logger,
		cfg:      cfg,
		down:     map[string]*downState{},
		describe: describe,
		send:     sendAll,
		sendSMS:  sendSMSEmail,
		now:      time.Now,
	}
}

func (n *notifier) config() NotifyConfig {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := n.cfg
	c.Watch = slices.Clone(c.Watch)
	if c.Channels != nil {
		ch := *c.Channels
		c.Channels = &ch
	}
	return c
}

func (n *notifier) setConfig(c NotifyConfig) {
	c.normalize()
	n.mu.Lock()
	n.cfg = c
	n.mu.Unlock()
}

func (n *notifier) currentStatus() notifyStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.status
}

func (n *notifier) watchedLocked(id string) bool {
	return n.cfg.AllAgents || slices.Contains(n.cfg.Watch, id)
}

// isWatched kaže da li se za agenta šalju obaveštenja.
func (n *notifier) isWatched(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.watchedLocked(id)
}

// startupOffline beleži agente koji su bili na vezi pre (re)starta konzole:
// ako se ne vrate u roku, i za njih stiže obaveštenje.
func (n *notifier) startupOffline(ids []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	for _, id := range ids {
		n.down[id] = &downState{since: now}
	}
}

// agentChanged se poziva iz hub-a kada se agent poveže ili izgubi vezu.
func (n *notifier) agentChanged(a AgentInfo) {
	if a.IsServer() {
		return // serveri imaju svoje praćenje (servers.go)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	id := a.AgentID
	st := n.down[id]
	if a.Online {
		if st != nil {
			delete(n.down, id)
			if st.alerted && n.cfg.Enabled && !n.cfg.SkipRecovery && n.watchedLocked(id) {
				n.ups = append(n.ups, upEvent{id: id, downFor: n.now().Sub(st.since)})
			}
		}
		return
	}
	if st == nil {
		n.down[id] = &downState{since: n.now()}
	}
}

// forget briše agenta (uklonjen iz konzole).
func (n *notifier) forget(id string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.down, id)
	n.ups = slices.DeleteFunc(n.ups, func(u upEvent) bool { return u.id == id })
}

func (n *notifier) run(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		n.tick()
	}
}

type downEvent struct {
	id    string
	since time.Time
}

// tick šalje obaveštenje za agente koji su van veze duže od zadatog vremena
// i za one koji su se u međuvremenu vratili.
func (n *notifier) tick() {
	n.sending.Lock()
	defer n.sending.Unlock()

	n.mu.Lock()
	cfg := n.cfg
	now := n.now()
	var downs []downEvent
	ups := n.ups
	n.ups = nil
	if !cfg.anyFor(scopeAgents) {
		n.mu.Unlock()
		return
	}
	for id, st := range n.down {
		if !st.alerted && now.Sub(st.since) >= cfg.delay() && n.watchedLocked(id) {
			st.alerted = true
			downs = append(downs, downEvent{id: id, since: st.since})
		}
	}
	n.mu.Unlock()

	if len(downs) == 0 && len(ups) == 0 {
		return
	}
	m := n.compose(downs, ups, cfg, now)
	m.Scope = scopeAgents
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	n.deliver(ctx, cfg, m)
}

// deliver šalje poruku na kanale koji su za nju uključeni (ili samo na
// m.Channel) i beleži ishod po kanalu.
func (n *notifier) deliver(ctx context.Context, cfg NotifyConfig, m notifyMessage) []channelResult {
	wants := func(key string) bool {
		if m.Channel != "" {
			return m.Channel == key
		}
		return cfg.sends(key, m.Scope)
	}
	var res []channelResult
	if wants(chEmail) || wants(chTeams) || wants(chNtfy) {
		res = n.send(ctx, cfg, m)
	}
	if wants(chSMS) {
		text := m.SMS
		if text == "" {
			text = smsFromMessage(m, n.now().Local().Format("15:04"))
		}
		res = append(res, newResult(chSMS, n.sendSMS(ctx, cfg, text)))
	}
	n.record(m, res)
	return res
}

func newResult(key string, err error) channelResult {
	r := channelResult{Key: key, Channel: channelLabels[key], OK: err == nil}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

// record beleži ishod slanja (ukupno i po kanalu).
func (n *notifier) record(m notifyMessage, res []channelResult) {
	if len(res) == 0 {
		return
	}
	var errs []string
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	if n.status.Channels == nil {
		n.status.Channels = map[string]*channelStatus{}
	}
	for _, r := range res {
		cs := n.status.Channels[r.Key]
		if cs == nil {
			cs = &channelStatus{}
			n.status.Channels[r.Key] = cs
		}
		if r.OK {
			cs.LastSent, cs.LastError = now, ""
		} else {
			cs.LastError, cs.LastErrorAt = r.Error, now
			errs = append(errs, r.Channel+": "+r.Error)
		}
	}
	if len(errs) < len(res) {
		n.status.LastSent = now
		n.status.LastTitle = m.Title
	}
	if len(errs) > 0 {
		n.status.LastError = strings.Join(errs, "; ")
		n.status.LastErrorAt = now
		n.log.Printf("obaveštenje %q: %s", m.Title, n.status.LastError)
	} else {
		n.status.LastError = ""
		n.log.Printf("obaveštenje poslato: %s", m.Title)
	}
}

func (n *notifier) label(id string) (host string, full string) {
	host, loc := n.describe(id)
	host = cleanText(host)
	if host == "" {
		host = id
	}
	full = host
	if loc = cleanText(loc); loc != "" {
		full += " (" + loc + ")"
	}
	return host, full
}

func (n *notifier) compose(downs []downEvent, ups []upEvent, cfg NotifyConfig, now time.Time) notifyMessage {
	type line struct{ host, text string }
	var downLines, upLines []line
	for _, d := range downs {
		host, full := n.label(d.id)
		downLines = append(downLines, line{host, fmt.Sprintf("%s – nije na vezi od %s", full, d.since.Local().Format("15:04"))})
	}
	for _, u := range ups {
		host, full := n.label(u.id)
		upLines = append(upLines, line{host, fmt.Sprintf("%s – nije bio na vezi %s", full, fmtDuration(u.downFor))})
	}
	byHost := func(a, b line) int { return strings.Compare(a.host, b.host) }
	slices.SortFunc(downLines, byHost)
	slices.SortFunc(upLines, byHost)

	var title string
	switch {
	case len(downLines) == 1 && len(upLines) == 0:
		title = downLines[0].host + " nije na vezi"
	case len(downLines) > 1 && len(upLines) == 0:
		title = fmt.Sprintf("%d računara nisu na vezi", len(downLines))
	case len(downLines) == 0 && len(upLines) == 1:
		title = upLines[0].host + " je ponovo na vezi"
	case len(downLines) == 0:
		title = fmt.Sprintf("%d računara su ponovo na vezi", len(upLines))
	default:
		title = fmt.Sprintf("Nije na vezi: %d, ponovo na vezi: %d", len(downLines), len(upLines))
	}
	var b strings.Builder
	if len(downLines) > 0 {
		fmt.Fprintf(&b, "Nisu na vezi duže od %s:\n", fmtDuration(cfg.delay()))
		for _, l := range downLines {
			b.WriteString("• " + l.text + "\n")
		}
	}
	if len(upLines) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Ponovo na vezi:\n")
		for _, l := range upLines {
			b.WriteString("• " + l.text + "\n")
		}
	}
	fmt.Fprintf(&b, "\nNetwork Monitor, %s", now.Local().Format("02.01.2006. 15:04"))
	return notifyMessage{Title: "Network Monitor: " + title, Body: b.String(), Down: len(downLines) > 0, Recovery: len(upLines) > 0}
}

// cleanText uklanja kontrolne znakove (naziv računara šalje agent, a ide u
// zaglavlja e-maila i HTTP zahteva).
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d < time.Minute:
		return "manje od minuta"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%d h", h)
		}
		return fmt.Sprintf("%d h %d min", h, m)
	}
}

// sendAll šalje poruku na e-mail, Teams i ntfy, ako su za nju uključeni
// (ili samo na m.Channel). SMS ide posebno (notifier.deliver).
func sendAll(ctx context.Context, cfg NotifyConfig, m notifyMessage) []channelResult {
	var res []channelResult
	for _, key := range []string{chEmail, chTeams, chNtfy} {
		if m.Channel != "" && m.Channel != key || m.Channel == "" && !cfg.sends(key, m.Scope) {
			continue
		}
		res = append(res, newResult(key, sendChannel(ctx, cfg, key, m)))
	}
	return res
}

func notifyHTTPClient(cfg NotifyConfig) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.Proxy != "" {
		if u, err := url.Parse(cfg.Proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

// ---------- HTTP API ----------

// notifyView je prikaz podešavanja u web interfejsu: lozinka i token se ne
// vraćaju, već samo informacija da su postavljeni.
type notifyView struct {
	Enabled      bool   `json:"enabled"`
	AllAgents    bool   `json:"all_agents"`
	DelayMinutes int    `json:"delay_minutes"`
	SkipRecovery bool   `json:"skip_recovery"`
	EmailHost    string `json:"email_host"`
	EmailPort    int    `json:"email_port"`
	EmailTLS     string `json:"email_tls"`
	EmailUser    string `json:"email_user"`
	EmailFrom    string `json:"email_from"`
	EmailTo      string `json:"email_to"`
	TeamsURL     string `json:"teams_url"`
	NtfyURL      string `json:"ntfy_url"`
	Proxy        string `json:"proxy"`
	SMSEmail     string `json:"sms_email"`
	SMSCheckTime string `json:"sms_check_time"`

	Channels *NotifyChannels `json:"channels"`

	SMSQueries       bool   `json:"sms_queries"`
	IMAPHost         string `json:"imap_host"`
	IMAPPort         int    `json:"imap_port"`
	SMSAliases       string `json:"sms_aliases"`
	SMSServerAliases string `json:"sms_server_aliases"`

	ServerDelayMinutes int `json:"server_delay_minutes"`
	ServerCPUPct       int `json:"server_cpu_pct"`
	ServerMemPct       int `json:"server_mem_pct"`
	ServerHighMinutes  int `json:"server_high_minutes"`

	EmailPasswordSet bool `json:"email_password_set"`
	NtfyTokenSet     bool `json:"ntfy_token_set"`
	// Samo pri snimanju: nil = bez promene, "" = briše.
	EmailPassword *string `json:"email_password,omitempty"`
	NtfyToken     *string `json:"ntfy_token,omitempty"`

	Watched int          `json:"watched"`
	Status  notifyStatus `json:"status"`
}

func (s *Server) notifyResponse() notifyView {
	c := s.notify.config()
	v := notifyView{
		Enabled: c.Enabled, AllAgents: c.AllAgents, DelayMinutes: int(c.delay() / time.Minute), SkipRecovery: c.SkipRecovery,
		EmailHost: c.EmailHost, EmailPort: c.EmailPort, EmailTLS: c.EmailTLS, EmailUser: c.EmailUser,
		EmailFrom: c.EmailFrom, EmailTo: c.EmailTo, TeamsURL: c.TeamsURL, NtfyURL: c.NtfyURL, Proxy: c.Proxy,
		SMSEmail: c.SMSEmail, SMSCheckTime: c.SMSCheckTime, Channels: c.Channels,
		SMSQueries: c.SMSQueries, IMAPHost: c.IMAPHost, IMAPPort: c.imapPort(), SMSAliases: c.aliasText(),
		SMSServerAliases:   c.SMSServerAliases,
		ServerDelayMinutes: int(c.serverDelay() / time.Minute), ServerCPUPct: int(c.serverCPU()), ServerMemPct: int(c.serverMem()),
		ServerHighMinutes: int(c.serverHigh() / time.Minute),
		EmailPasswordSet:  c.EmailPassword != "", NtfyTokenSet: c.NtfyToken != "",
		Watched: len(c.Watch), Status: s.notify.currentStatus(),
	}
	if v.EmailPort == 0 {
		v.EmailPort = 25
	}
	return v
}

func (s *Server) handleGetNotify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.notifyResponse())
}

func (s *Server) handleSetNotify(w http.ResponseWriter, r *http.Request) {
	var v notifyView
	if err := decodeBody(r, &v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	c := s.notify.config()
	c.Enabled, c.AllAgents, c.DelayMinutes, c.SkipRecovery = v.Enabled, v.AllAgents, v.DelayMinutes, v.SkipRecovery
	c.EmailHost, c.EmailPort, c.EmailTLS = strings.TrimSpace(v.EmailHost), v.EmailPort, v.EmailTLS
	c.EmailUser, c.EmailFrom, c.EmailTo = strings.TrimSpace(v.EmailUser), strings.TrimSpace(v.EmailFrom), strings.TrimSpace(v.EmailTo)
	c.TeamsURL, c.NtfyURL, c.Proxy = strings.TrimSpace(v.TeamsURL), strings.TrimSpace(v.NtfyURL), strings.TrimSpace(v.Proxy)
	c.SMSEmail, c.SMSCheckTime = strings.TrimSpace(v.SMSEmail), strings.TrimSpace(v.SMSCheckTime)
	c.SMSQueries, c.IMAPHost, c.IMAPPort = v.SMSQueries, strings.TrimSpace(v.IMAPHost), v.IMAPPort
	c.SMSAliases = strings.TrimSpace(strings.ReplaceAll(v.SMSAliases, "\r", ""))
	c.SMSServerAliases = strings.TrimSpace(strings.ReplaceAll(v.SMSServerAliases, "\r", ""))
	c.ServerDelayMinutes, c.ServerCPUPct, c.ServerMemPct, c.ServerHighMinutes = v.ServerDelayMinutes, v.ServerCPUPct, v.ServerMemPct, v.ServerHighMinutes
	if c.SMSAliases == defaultAliases {
		c.SMSAliases = "" // podrazumevane se ne upisuju
	}
	if c.IMAPPort == 993 {
		c.IMAPPort = 0 // podrazumevano
	}
	if v.Channels != nil {
		ch := *v.Channels
		c.Channels = &ch
	}
	if v.EmailPassword != nil {
		c.EmailPassword = *v.EmailPassword
	}
	if v.NtfyToken != nil {
		c.NtfyToken = strings.TrimSpace(*v.NtfyToken)
	}
	if c.EmailUser == "" {
		c.EmailPassword = ""
	}
	if err := c.validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.persist(func(fc *FileConfig) { cp := c; fc.Notify = &cp }); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.notify.setConfig(c)
	var on []string
	for _, k := range channelKeys {
		if c.Channels.get(k).On {
			on = append(on, channelLabels[k])
		}
	}
	s.log.Printf("podešavanja obaveštenja promenjena (uključena: %v, kanali: %s)", c.Enabled, strings.Join(on, ", "))
	writeJSON(w, http.StatusOK, s.notifyResponse())
}

// handleTestNotify šalje probno obaveštenje na jedan kanal. Radi i kada je
// kanal (ili glavni prekidač) isključen, da bi se podešavanja proverila pre
// uključivanja.
func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Channel string `json:"channel"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !slices.Contains(channelKeys, req.Channel) {
		writeErr(w, http.StatusBadRequest, errors.New("nepoznat kanal"))
		return
	}
	c := s.notify.config()
	if !c.configured(req.Channel) {
		need := map[string]string{
			chEmail: "SMTP server i bar jedan primalac",
			chSMS:   "SMTP server i sanduče SMS telefona",
			chTeams: "adresa Teams webhook-a",
			chNtfy:  "adresa ntfy teme",
		}[req.Channel]
		writeErr(w, http.StatusBadRequest, fmt.Errorf("kanal %s nije podešen (potrebno: %s)", channelLabels[req.Channel], need))
		return
	}
	user := identityFrom(r).Name
	now := time.Now()
	m := notifyMessage{
		Channel: req.Channel,
		Title:   "Network Monitor: probno obaveštenje",
		Body: fmt.Sprintf("Ovo je probno obaveštenje koje je poslao korisnik %s.\nAko ste ga dobili, obaveštenja rade.\n\nNetwork Monitor, %s",
			user, now.Format("02.01.2006. 15:04")),
		SMS: truncSMS(smsASCII(fmt.Sprintf("PROBA NetMon %s - probni SMS (%s)", now.Format("15:04"), user))),
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{"results": s.notify.deliver(ctx, c, m)})
}

// handleWatch uključuje ili isključuje obaveštenja za jednog agenta.
func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Watch bool `json:"watch"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	if _, ok := s.hub.Agent(id); !ok {
		writeErr(w, http.StatusNotFound, errors.New("nepoznat agent"))
		return
	}
	if err := s.setWatch(id, req.Watch); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"watch": req.Watch})
}

func (s *Server) setWatch(id string, on bool) error {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	c := s.notify.config()
	c.Watch = slices.DeleteFunc(c.Watch, func(x string) bool { return x == id })
	if on {
		c.Watch = append(c.Watch, id)
		sort.Strings(c.Watch)
	}
	if err := s.persist(func(fc *FileConfig) { cp := c; fc.Notify = &cp }); err != nil {
		return err
	}
	s.notify.setConfig(c)
	return nil
}
