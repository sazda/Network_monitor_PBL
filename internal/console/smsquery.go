package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// SMS upiti: na SMS „status <lokacija>” telefon (aplikacija NetMon SMS)
// šalje e-mail sa oznakom queryTag u naslovu na sanduče SMS telefona;
// konzola ga čita (IMAP) i odgovara kao običan SMS e-mail sa brojem upita
// („[NETMON-SMS] #1234 …”). Broj telefona nikad ne ide u e-mail: telefon
// pamti ko je poslao upit i odgovor šalje samo njemu.
const (
	queryTag      = "[NETMON-UPIT]"
	queryEvery    = 20 * time.Second
	queryMaxAge   = 10 * time.Minute // stariji upiti se samo brišu
	queryReplyMax = 300              // najviše dva SMS-a
)

var queryRe = regexp.MustCompile(`(?i)^\s*\[NETMON-UPIT\]\s*#(\d{1,9})\s+(.*)$`)

// smsQuery je jedan upit iz sanduča.
type smsQuery struct {
	ID   string
	Text string // npr. „status sever”
	From string
	Date time.Time
}

// parseQuery čita broj upita i tekst iz naslova.
func parseQuery(subject string) (id, text string, ok bool) {
	m := queryRe.FindStringSubmatch(subject)
	if m == nil {
		return "", "", false
	}
	return m[1], strings.TrimSpace(m[2]), true
}

// normName: mala slova, bez naših slova i znakova (za poređenje naziva).
func normName(s string) string {
	s = strings.ToLower(smsASCII(s))
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// findLocation traži lokaciju po nazivu: tačno, pa početak naziva, pa deo
// naziva (bez obzira na velika slova i naša slova).
func findLocation(locs []*locSummary, q string) (*locSummary, []string) {
	nq := normName(q)
	if nq == "" {
		return nil, nil
	}
	var prefix, part []*locSummary
	for _, l := range locs {
		n := normName(l.Name)
		switch {
		case n == nq:
			return l, nil
		case strings.HasPrefix(n, nq):
			prefix = append(prefix, l)
		case strings.Contains(n, nq):
			part = append(part, l)
		}
	}
	for _, list := range [][]*locSummary{prefix, part} {
		if len(list) == 1 {
			return list[0], nil
		}
		if len(list) > 1 {
			var names []string
			for _, l := range list {
				names = append(names, l.Name)
			}
			return nil, names
		}
	}
	return nil, nil
}

// defaultAliases: skraćenice lokacija za SMS upite dok ih administrator ne promeni.
const defaultAliases = "GC = Glavni Centar\nFI = Filijala\nJU = Jug\nZP = Zapad\nSE = Sever\nIS = Istok\nDO = Dolina\nBR = Brdo"

// parseAliases čita skraćenice („GC = Glavni Centar”, jedna po redu) u
// mapu normalizovana skraćenica → naziv lokacije.
func parseAliases(text string) (map[string]string, []string, error) {
	return parseAliasList(text, "skraćenice", "GC = Glavni Centar")
}

// parseServerAliases čita skraćenice servera („SOL = srv-monitor”).
func parseServerAliases(text string) (map[string]string, []string, error) {
	return parseAliasList(text, "skraćenice servera", "SOL = srv-monitor")
}

func parseAliasList(text, what, example string) (map[string]string, []string, error) {
	out := map[string]string{}
	var order []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		code, name, ok := strings.Cut(line, "=")
		code, name = strings.TrimSpace(code), strings.TrimSpace(name)
		n := normName(code)
		if !ok || n == "" || name == "" || len(n) > 8 || strings.ContainsAny(code, " \t") {
			return nil, nil, fmt.Errorf("%s, red %d: očekuje se „%s” (skraćenica do 8 slova i cifara)", what, i+1, example)
		}
		if _, dup := out[n]; dup {
			return nil, nil, fmt.Errorf("%s, red %d: %s je već upisana", what, i+1, code)
		}
		out[n] = name
		order = append(order, strings.ToUpper(code))
	}
	return out, order, nil
}

func (c *NotifyConfig) aliasText() string {
	if strings.TrimSpace(c.SMSAliases) == "" {
		return defaultAliases
	}
	return c.SMSAliases
}

// smsNL: oznaka za novi red u odgovoru na upit. Tekst putuje u naslovu
// e-maila (bez novih redova); aplikacija na telefonu (build 36+) oznaku
// pretvara u pravi novi red pre slanja SMS-a.
const smsNL = `\n`

// queryHelp: odgovor na „status” i na nepoznatu komandu – spisak komandi.
const queryHelp = "NetMon komande:" + smsNL +
	"lokacija lista - spisak lokacija" + smsNL +
	"lokacija <broj ili naziv> - izvestaj o lokaciji" + smsNL +
	"server lista - spisak servera" + smsNL +
	"server <broj ili naziv> - izvestaj o serveru" + smsNL +
	"mpls lista - spisak MPLS veza"

// isList: „lista” / „list” kao drugi deo komande.
func isList(s string) bool { return strings.EqualFold(s, "lista") || strings.EqualFold(s, "list") }

// answerQuery sastavlja odgovor na upit (bez broja upita); aliases su
// skraćenice lokacija („GC = Glavni Centar”), srvAliases skraćenice
// servera („SOL = srv-monitor”). Komande: lokacija (lokacije), server,
// mpls; „status” i nepoznata komanda vraćaju spisak komandi.
func answerQuery(ov netdevResponse, srv serversResponse, text string, now time.Time, aliases, srvAliases string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return queryHelp
	}
	arg := strings.Join(words[1:], " ")
	switch strings.ToLower(words[0]) {
	case "lokacija", "lokacije":
		return answerLocation(ov, arg, now, aliases)
	case "server", "serveri":
		switch {
		case isList(arg):
			// Spisak svih servera: bez ograničenja dužine (telefon ga šalje
			// kao jednu spojenu poruku od više delova).
			return smsASCII(serverList(srv, now))
		case arg == "":
			return truncQuery(smsASCII(answerServer(srv, arg, now, srvAliases))) // kratak pregled svih
		}
		// Izveštaj o jednom serveru (naziv, skraćenica ili redni broj): ceo.
		return smsASCII(answerServer(srv, arg, now, srvAliases))
	case "mpls":
		return "MPLS: izvestaj jos nije dostupan. Ostale komande: posaljite status"
	}
	return queryHelp
}

// answerLocation: „lokacija” (kratak pregled), „lokacija lista”,
// „lokacija <broj | naziv | skraćenica>” (pun izveštaj).
func answerLocation(ov netdevResponse, arg string, now time.Time, aliases string) string {
	alias, codes, _ := parseAliases(aliases)
	at := now.Local().Format("15:04")
	if arg == "" {
		return truncQuery(smsASCII("LOKACIJE " + at + ": " + summaryAll(ov, alias)))
	}
	if isList(arg) {
		// Spisak lokacija sa rednim brojevima, bez ograničenja dužine.
		return smsASCII(locationList(ov, alias, now))
	}
	if n, err := strconv.Atoi(arg); err == nil {
		// Redni broj sa spiska: pun izveštaj, sa punim nazivom (brojevi se
		// menjaju kada se doda lokacija).
		if n < 1 || n > len(ov.Locations) {
			return smsASCII(fmt.Sprintf("LOKACIJA: nema lokacije broj %d (lokacija: %d). Spisak: lokacija lista", n, len(ov.Locations)))
		}
		l := ov.Locations[n-1]
		return smsASCII(fmt.Sprintf("LOKACIJA %s %s: %s", l.Name, at, locationReport(ov, l)))
	}
	var loc *locSummary
	var similar []string
	if name, ok := alias[normName(arg)]; ok {
		// Skraćenica: tačan naziv lokacije.
		for _, l := range ov.Locations {
			if normName(l.Name) == normName(name) {
				loc = l
			}
		}
		if loc == nil {
			return truncQuery(smsASCII(fmt.Sprintf("LOKACIJA %s: lokacija %s nema uredjaja u konzoli", strings.ToUpper(arg), name)))
		}
	} else {
		loc, similar = findLocation(ov.Locations, arg)
	}
	if loc == nil {
		msg := fmt.Sprintf("LOKACIJA: nepoznata lokacija %q.", arg)
		if len(similar) > 0 {
			msg += " Da li ste mislili: " + strings.Join(similar, ", ") + "?"
		} else if len(codes) > 0 {
			msg += " Skracenice: " + strings.Join(codes, ", ") + ". Spisak: lokacija lista"
		} else {
			msg += " Spisak: lokacija lista"
		}
		return truncQuery(smsASCII(msg))
	}
	// Pun izveštaj, bez ograničenja na dva SMS-a.
	return smsASCII(fmt.Sprintf("LOKACIJA %s %s: %s", loc.Name, at, locationReport(ov, loc)))
}

// sortedServers: serveri abecednim redom (redni brojevi za „server lista” i
// „server <broj>”).
func sortedServers(srv serversResponse) []serverView {
	out := slices.Clone(srv.Servers)
	slices.SortStableFunc(out, func(a, b serverView) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}

// serverList: „server lista” – puni nazivi svih servera u konzoli, bez obzira
// na stanje, abecednim redom, sa rednim brojem (za „server <broj>”).
func serverList(srv serversResponse, now time.Time) string {
	if len(srv.Servers) == 0 {
		return "SERVERI: nema servera u konzoli"
	}
	list := sortedServers(srv)
	items := make([]string, 0, len(list))
	for i, v := range list {
		items = append(items, fmt.Sprintf("%d %s", i+1, v.Name))
	}
	return fmt.Sprintf("SERVERI %s (%d):%s%s%sIzvestaj: server <broj>", now.Local().Format("15:04"), len(items),
		smsNL, strings.Join(items, smsNL), smsNL)
}

// answerServer: „server <naziv ili skraćenica>” (stanje jednog servera) ili
// „server” (svi).
func answerServer(srv serversResponse, name string, now time.Time, aliases string) string {
	at := now.Local().Format("15:04")
	if name == "" {
		if len(srv.Servers) == 0 {
			return "SERVERI: nema servera u konzoli"
		}
		on := 0
		var off, high []string
		for _, v := range srv.Servers {
			if v.Online {
				on++
			} else {
				s := v.Name
				if !v.Since.IsZero() {
					s += " od " + v.Since.Local().Format("15:04")
				}
				off = append(off, s)
			}
			if v.CPUHigh || v.MemHigh {
				high = append(high, v.Name)
			}
		}
		out := fmt.Sprintf("SERVERI %s: %d/%d online", at, on, len(srv.Servers))
		if len(off) > 0 {
			out += "; offline: " + strings.Join(off, ", ")
		}
		if len(high) > 0 {
			out += "; visoko opterecenje: " + strings.Join(high, ", ")
		}
		return out
	}
	if n, err := strconv.Atoi(name); err == nil {
		// Redni broj sa spiska „server lista”.
		list := sortedServers(srv)
		if n < 1 || n > len(list) {
			return fmt.Sprintf("SERVER: nema servera broj %d (servera: %d). Spisak: server lista", n, len(list))
		}
		return serverReport(&list[n-1], now)
	}
	alias, codes, _ := parseServerAliases(aliases)
	if full, ok := alias[normName(name)]; ok {
		// Skraćenica: tačan naziv servera.
		for i := range srv.Servers {
			if normName(srv.Servers[i].Name) == normName(full) {
				return serverReport(&srv.Servers[i], now)
			}
		}
		return fmt.Sprintf("SERVER %s: server %s nije u konzoli", strings.ToUpper(name), full)
	}
	var v *serverView
	var similar []string
	for i := range srv.Servers {
		n := normName(srv.Servers[i].Name)
		if n == normName(name) {
			v = &srv.Servers[i]
			break
		}
		if strings.HasPrefix(n, normName(name)) {
			similar = append(similar, srv.Servers[i].Name)
		}
	}
	if v == nil {
		msg := fmt.Sprintf("SERVER: nepoznat server %q.", name)
		if len(similar) > 0 {
			msg += " Da li ste mislili: " + strings.Join(similar, ", ") + "?"
		} else if len(codes) > 0 {
			msg += " Skracenice: " + strings.Join(codes, ", ")
		} else if len(srv.Servers) > 0 {
			var all []string
			for _, x := range srv.Servers {
				all = append(all, x.Name)
			}
			msg += " Serveri: " + strings.Join(all, ", ")
		}
		return msg
	}
	return serverReport(v, now)
}

// serverReport: stanje jednog servera za SMS.
func serverReport(v *serverView, now time.Time) string {
	at := now.Local().Format("15:04")
	if !v.Online {
		out := fmt.Sprintf("SERVER %s %s: OFFLINE", v.Name, at)
		if !v.Since.IsZero() {
			out += fmt.Sprintf(" od %s (%s)", v.Since.Local().Format("15:04"), fmtDuration(now.Sub(v.Since)))
		}
		return out
	}
	out := fmt.Sprintf("SERVER %s %s: online", v.Name, at)
	if m := v.Metrics; m != nil {
		out += fmt.Sprintf(", uptime %s, CPU %.0f%%", fmtUptimeShort(m.UptimeSec), m.CPUPct)
		if v.CPUHigh {
			out += " (VISOK)"
		}
		if m.MemTotal > 0 {
			out += fmt.Sprintf(", RAM %.0f%% (%s/%s)", 100*float64(m.MemUsed)/float64(m.MemTotal), fmtGB(m.MemUsed), fmtGB(m.MemTotal))
			if v.MemHigh {
				out += " (VISOK)"
			}
		}
		var ds []string
		for _, d := range m.Disks {
			if d.Total > 0 {
				ds = append(ds, fmt.Sprintf("%s %.0f%% (slobodno %s)", d.Name, 100*float64(d.Total-d.Free)/float64(d.Total), fmtGB(d.Free)))
			}
		}
		if len(ds) > 0 {
			out += ", disk " + strings.Join(ds, ", ")
		}
	} else {
		out += " (stanje jos nije stiglo)"
	}
	return out
}

func fmtUptimeShort(sec uint64) string {
	d, h, m := sec/86400, sec%86400/3600, sec%3600/60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dmin", h, m)
	}
	return fmt.Sprintf("%dmin", m)
}

func fmtGB(b uint64) string {
	g := float64(b) / (1 << 30)
	if g >= 10 {
		return fmt.Sprintf("%.0f GB", g)
	}
	return fmt.Sprintf("%.1f GB", g)
}

// locationReport: „sve operativno (3/3)” ili spisak uređaja koji ne rade.
func locationReport(ov netdevResponse, loc *locSummary) string {
	var down []string
	for _, d := range ov.Devices {
		if d.LocationName != loc.Name || d.Status != stDown {
			continue
		}
		s := d.Name + " offline"
		if !d.Since.IsZero() {
			s += " od " + d.Since.Local().Format("15:04")
		}
		down = append(down, s)
	}
	sort.Strings(down)
	var b strings.Builder
	switch loc.Status {
	case stLocDown:
		b.WriteString("LOKACIJA NEDOSTUPNA")
		if !loc.Since.IsZero() {
			b.WriteString(" od " + loc.Since.Local().Format("15:04"))
		}
		b.WriteString(" (nijedan uredjaj ne odgovara)")
		return b.String()
	case stBackup:
		b.WriteString("radi preko REZERVNE veze (MPLS ne radi)")
	}
	if len(down) == 0 {
		if b.Len() == 0 {
			if loc.Total == 0 {
				return "nema aktivnih uredjaja"
			}
			return fmt.Sprintf("sve operativno (%d/%d na vezi)", loc.Up, loc.Total)
		}
		return b.String()
	}
	if b.Len() > 0 {
		b.WriteString("; ")
	}
	b.WriteString(strings.Join(down, ", "))
	fmt.Fprintf(&b, " (%d/%d na vezi)", loc.Up, loc.Total)
	return b.String()
}

// locationList: „lokacija lista” – lokacije koje trenutno imaju mrežnu opremu u
// konzoli, abecednim redom, sa rednim brojem (za „lokacija <broj>”) i
// skraćenicom ako postoji.
func locationList(ov netdevResponse, alias map[string]string, now time.Time) string {
	if len(ov.Locations) == 0 {
		return "LOKACIJE: nema mrezne opreme u konzoli"
	}
	short := map[string]string{} // naziv → skraćenica
	for code, name := range alias {
		short[normName(name)] = strings.ToUpper(code)
	}
	items := make([]string, 0, len(ov.Locations))
	for i, l := range ov.Locations {
		s := fmt.Sprintf("%d %s", i+1, l.Name)
		if c, ok := short[normName(l.Name)]; ok && normName(c) != normName(l.Name) {
			s += " (" + c + ")"
		}
		items = append(items, s)
	}
	return fmt.Sprintf("LOKACIJE %s (%d):%s%s%sIzvestaj: lokacija <broj>", now.Local().Format("15:04"), len(items),
		smsNL, strings.Join(items, smsNL), smsNL)
}

// summaryAll: kratak pregled svih lokacija (komanda „status” bez lokacije).
func summaryAll(ov netdevResponse, alias map[string]string) string {
	short := map[string]string{} // naziv → skraćenica (da više stane u SMS)
	for code, name := range alias {
		short[normName(name)] = strings.ToUpper(code)
	}
	var bad []string
	for _, l0 := range ov.Locations {
		l := *l0
		if c, ok := short[normName(l.Name)]; ok {
			l.Name = c
		}
		switch l.Status {
		case stLocDown:
			bad = append(bad, l.Name+" nedostupna")
		case stBackup:
			bad = append(bad, l.Name+" rezervna veza")
		case stPartial:
			bad = append(bad, fmt.Sprintf("%s %d offline", l.Name, l.Down))
		}
	}
	if len(bad) == 0 {
		return fmt.Sprintf("sve lokacije operativne (%d)", len(ov.Locations))
	}
	return "problemi - " + strings.Join(bad, "; ")
}

func truncQuery(s string) string {
	if len(s) <= queryReplyMax {
		return s
	}
	return s[:queryReplyMax-3] + "..."
}

// ---------- čitanje sanduča ----------

func (c *NotifyConfig) imapHost() string {
	if c.IMAPHost != "" {
		return c.IMAPHost
	}
	return c.EmailHost
}

func (c *NotifyConfig) imapPort() int {
	if c.IMAPPort > 0 {
		return c.IMAPPort
	}
	return 993
}

// queriesEnabled: upiti se obrađuju ako su uključeni i postoje SMTP nalog
// (za čitanje sanduča i slanje odgovora) i sanduče SMS telefona.
func (c *NotifyConfig) queriesEnabled() bool {
	return c.SMSQueries && c.emailEnabled() && c.SMSEmail != "" && c.EmailUser != "" && c.EmailPassword != ""
}

// imapRootCAs: nil = sistemski sertifikati (menja se samo u testovima).
var imapRootCAs *x509.CertPool

// fetchQueries čita neobrađene upite iz sanduča, briše ih i vraća one koji
// su stigli sa adrese SMS telefona i nisu prestari.
func fetchQueries(ctx context.Context, cfg NotifyConfig, now time.Time) ([]smsQuery, error) {
	addr := net.JoinHostPort(cfg.imapHost(), strconv.Itoa(cfg.imapPort()))
	d := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: cfg.imapHost(), MinVersion: tls.VersionTLS12, RootCAs: imapRootCAs})
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := client.New(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	defer c.Logout()
	if err := c.Login(cfg.EmailUser, cfg.EmailPassword); err != nil {
		return nil, fmt.Errorf("prijava na sanduče: %w", err)
	}
	if _, err := c.Select("INBOX", false); err != nil {
		return nil, err
	}
	crit := imap.NewSearchCriteria()
	crit.WithoutFlags = []string{imap.SeenFlag, imap.DeletedFlag}
	crit.Header.Add("Subject", queryTag)
	uids, err := c.UidSearch(crit)
	if err != nil || len(uids) == 0 {
		return nil, err
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	msgs := make(chan *imap.Message, len(uids))
	if err := c.UidFetch(set, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, msgs); err != nil {
		return nil, err
	}
	phone := ""
	if a, err := mail.ParseAddress(cfg.SMSEmail); err == nil {
		phone = strings.ToLower(a.Address)
	}
	var out []smsQuery
	for m := range msgs {
		if m.Envelope == nil {
			continue
		}
		id, text, ok := parseQuery(m.Envelope.Subject)
		if !ok {
			continue
		}
		from := ""
		if len(m.Envelope.From) > 0 {
			from = strings.ToLower(m.Envelope.From[0].Address())
		}
		if from != phone || now.Sub(m.Envelope.Date) > queryMaxAge {
			continue // tuđ ili star upit: samo se briše
		}
		out = append(out, smsQuery{ID: id, Text: text, From: from, Date: m.Envelope.Date})
	}
	// Obrađeni (i odbačeni) upiti se brišu iz sanduča.
	if err := c.UidStore(set, imap.FormatFlagsOp(imap.AddFlags, true), []interface{}{imap.SeenFlag, imap.DeletedFlag}, nil); err != nil {
		return out, err
	}
	return out, c.Expunge(nil)
}

// queryLoop obrađuje SMS upite dok konzola radi.
func (s *Server) queryLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		s.processQueries()
	}
}

func (s *Server) processQueries() {
	cfg := s.notify.config()
	if !cfg.queriesEnabled() || s.net == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	qs, err := s.fetchQueries(ctx, cfg, time.Now())
	s.notify.recordQuery(err)
	if err != nil {
		s.log.Printf("SMS upiti: %v", err)
	}
	for _, q := range qs {
		ans := answerQuery(s.net.overview(), s.srv.list(nil), q.Text, time.Now(), cfg.aliasText(), cfg.SMSServerAliases)
		text := "#" + q.ID + " " + ans
		s.log.Printf("SMS upit #%s %q → %s", q.ID, q.Text, ans)
		if err := s.notify.sendSMS(ctx, cfg, text); err != nil {
			s.log.Printf("odgovor na SMS upit #%s: %v", q.ID, err)
			s.notify.recordQuery(err)
		}
	}
}

// recordQuery beleži ishod poslednjeg čitanja upita (prikaz u podešavanjima).
func (n *notifier) recordQuery(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		n.status.QueryError, n.status.QueryErrorAt = err.Error(), n.now()
		return
	}
	n.status.QueryAt, n.status.QueryError = n.now(), ""
}

func (c *NotifyConfig) validateQueries() error {
	if _, _, err := parseAliases(c.SMSAliases); err != nil {
		return err
	}
	if _, _, err := parseServerAliases(c.SMSServerAliases); err != nil {
		return err
	}
	if !c.SMSQueries {
		return nil
	}
	if c.SMSEmail == "" || !c.emailEnabled() {
		return errors.New("SMS upiti traže SMTP server i sanduče SMS telefona")
	}
	if c.EmailUser == "" {
		return errors.New("SMS upiti traže nalog za SMTP (isti nalog čita sanduče preko IMAP-a)")
	}
	if c.IMAPPort < 0 || c.IMAPPort > 65535 {
		return errors.New("neispravan IMAP port")
	}
	return nil
}
