package console

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

func testOverview() netdevResponse {
	since := time.Date(2026, 11, 10, 9, 12, 0, 0, time.Local)
	return netdevResponse{
		Locations: []*locSummary{
			{Name: "Glavni Centar", Status: stOK, Total: 3, Up: 3},
			{Name: "Sever", Status: stBackup, Total: 3, Up: 2, Down: 1, Since: since},
			{Name: "Dolina", Status: stPartial, Total: 2, Up: 1, Down: 1},
			{Name: "Zapad", Status: stOK, Total: 1, Up: 1},
			{Name: "Istok", Status: stLocDown, Total: 1, Down: 1, Since: since},
		},
		Devices: []deviceView{
			{NetDevice: NetDevice{Name: "MPLS ruter"}, LocationName: "Sever", Status: stDown, Since: since},
			{NetDevice: NetDevice{Name: "NI_SW2"}, LocationName: "Dolina", Status: stDown, Since: since},
			{NetDevice: NetDevice{Name: "HQ_DC_S-I"}, LocationName: "Glavni Centar", Status: stUp},
		},
	}
}

func TestAnswerQuery(t *testing.T) {
	ov := testOverview()
	now := time.Date(2026, 11, 10, 10, 42, 0, 0, time.Local)
	cases := map[string]string{
		"lokacija glavni centar": "LOKACIJA Glavni Centar 10:42: sve operativno (3/3 na vezi)",
		"LOKACIJA Sever":         "LOKACIJA Sever 10:42: radi preko REZERVNE veze (MPLS ne radi); MPLS ruter offline od 09:12 (2/3 na vezi)",
		"lokacija dolina":        "LOKACIJA Dolina 10:42: NI_SW2 offline od 09:12 (1/2 na vezi)",
		"Lokacija ist":           "LOKACIJA Istok 10:42: LOKACIJA NEDOSTUPNA od 09:12 (nijedan uredjaj ne odgovara)",
		"lokacija":               "LOKACIJE 10:42: problemi - SE rezervna veza; DO 1 offline; IS nedostupna",
		"lokacija gc":            "LOKACIJA Glavni Centar 10:42: sve operativno (3/3 na vezi)",
		"LOKACIJA ZP":            "LOKACIJA Zapad 10:42: sve operativno (1/1 na vezi)",
		"lokacija do":            "LOKACIJA Dolina 10:42: NI_SW2 offline od 09:12 (1/2 na vezi)",
		"lokacija br":            "LOKACIJA BR: lokacija Brdo nema uredjaja u konzoli",
		"lokacija zap":           "LOKACIJA Zapad 10:42: sve operativno (1/1 na vezi)",
		"lokacija n":             `LOKACIJA: nepoznata lokacija "n". Da li ste mislili: Glavni Centar, Dolina?`,
		"lokacija beograd":       `LOKACIJA: nepoznata lokacija "beograd". Skracenice: GC, FI, JU, ZP, SE, IS, DO, BR. Spisak: lokacija lista`,
		"zdravo":                 queryHelp,
	}
	for q, want := range cases {
		if got := answerQuery(ov, serversResponse{}, q, now, defaultAliases, ""); got != want {
			t.Errorf("%q:\n got %q\nwant %q", q, got, want)
		}
	}
	if id, text, ok := parseQuery("[NETMON-UPIT] #4711 status Sever"); !ok || id != "4711" || text != "status Sever" {
		t.Fatalf("parse: %q %q %v", id, text, ok)
	}
	if _, _, ok := parseQuery("Re: [NETMON-UPIT] status"); ok {
		t.Fatal("bez broja upita")
	}
	// Bez skraćenica: lista lokacija.
	if got := answerQuery(ov, serversResponse{}, "lokacija beograd", now, "#", ""); !strings.Contains(got, "Spisak: lokacija lista") {
		t.Fatalf("bez skraćenica: %q", got)
	}
	for _, bad := range []string{"PS Glavni Centar", "PS = Glavni Centar\nps = Sever", "P S = X", "= X"} {
		if _, _, err := parseAliases(bad); err == nil {
			t.Errorf("očekivana greška za %q", bad)
		}
	}
}

func TestProcessQueries(t *testing.T) {
	s, _ := newTestServer(t)
	cfg := NotifyConfig{EmailHost: "mail", EmailFrom: "a@b.rs", EmailUser: "netmon@b.rs", EmailPassword: "x", SMSEmail: "netmon@b.rs", SMSQueries: true}
	s.notify.setConfig(cfg)
	s.fetchQueries = func(context.Context, NotifyConfig, time.Time) ([]smsQuery, error) {
		return []smsQuery{{ID: "12", Text: "lokacija"}}, nil
	}
	var sent []string
	s.notify.sendSMS = func(_ context.Context, _ NotifyConfig, text string) error { sent = append(sent, text); return nil }
	s.processQueries()
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "#12 LOKACIJE ") {
		t.Fatalf("odgovor: %q", sent)
	}
	if st := s.notify.currentStatus(); st.QueryAt.IsZero() || st.QueryError != "" {
		t.Fatalf("stanje: %+v", st)
	}
	// Isključeno: ništa se ne čita.
	cfg.SMSQueries = false
	s.notify.setConfig(cfg)
	s.fetchQueries = func(context.Context, NotifyConfig, time.Time) ([]smsQuery, error) {
		t.Fatal("ne sme da čita")
		return nil, nil
	}
	s.processQueries()
}

// Test sa pravim IMAP serverom (Dovecot): NETMON_TEST_IMAP=host:port:nalog:lozinka
// i NETMON_TEST_IMAP_CA=putanja do sertifikata servera.
func TestFetchQueriesIMAP(t *testing.T) {
	env := os.Getenv("NETMON_TEST_IMAP")
	if env == "" {
		t.Skip("NETMON_TEST_IMAP nije zadat")
	}
	p := strings.Split(env, ":")
	port, _ := strconv.Atoi(p[1])
	pem, err := os.ReadFile(os.Getenv("NETMON_TEST_IMAP_CA"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pem)
	imapRootCAs = pool
	defer func() { imapRootCAs = nil }()

	// Ubaci poruke direktno (APPEND).
	c, err := client.DialTLS(p[0]+":"+p[1], &tls.Config{RootCAs: pool, ServerName: p[0]})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(p[2], p[3]); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	add := func(from, subject string, at time.Time) {
		msg := "From: " + from + "\r\nTo: netmon@firma.rs\r\nSubject: " + subject + "\r\nDate: " + at.Format(time.RFC1123Z) + "\r\n\r\nx\r\n"
		if err := c.Append("INBOX", nil, at, strings.NewReader(msg)); err != nil {
			t.Fatal(err)
		}
	}
	add("NetMon SMS <netmon@firma.rs>", "[NETMON-UPIT] #77 status Sever", now)
	add("napadac@drugi.rs", "[NETMON-UPIT] #78 status", now)
	add("netmon@firma.rs", "[NETMON-UPIT] #79 status", now.Add(-time.Hour))
	add("netmon@firma.rs", "[NETMON-SMS] PAD SW1", now)
	c.Logout()

	cfg := NotifyConfig{EmailHost: p[0], IMAPPort: port, EmailUser: p[2], EmailPassword: p[3], SMSEmail: "netmon@firma.rs"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	qs, err := fetchQueries(ctx, cfg, now)
	if err != nil || len(qs) != 1 || qs[0].ID != "77" || qs[0].Text != "status Sever" {
		t.Fatalf("upiti: %+v %v", qs, err)
	}
	// Upiti su obrisani, SMS poruka za telefon ostaje netaknuta.
	qs, err = fetchQueries(ctx, cfg, now)
	if err != nil || len(qs) != 0 {
		t.Fatalf("drugi put: %+v %v", qs, err)
	}
	c2, _ := client.DialTLS(p[0]+":"+p[1], &tls.Config{RootCAs: pool, ServerName: p[0]})
	c2.Login(p[2], p[3])
	c2.Select("INBOX", true)
	crit := imap.NewSearchCriteria()
	crit.Header.Add("Subject", "NETMON")
	left, _ := c2.Search(crit)
	crit.WithoutFlags = []string{imap.SeenFlag}
	unseen, _ := c2.Search(crit)
	c2.Logout()
	if len(left) != 1 || len(unseen) != 1 {
		t.Fatalf("u sanduču: NETMON poruka %d, nepročitanih %d", len(left), len(unseen))
	}
}

func TestStatusListAndNumber(t *testing.T) {
	ov := testOverview()
	now := time.Date(2026, 11, 10, 10, 42, 0, 0, time.Local)
	want := `LOKACIJE 10:42 (5):\n1 Glavni Centar (GC)\n2 Sever (SE)\n3 Dolina (DO)\n4 Zapad (ZP)\n5 Istok (IS)\nIzvestaj: lokacija <broj>`
	for _, q := range []string{"lokacija lista", "LOKACIJE List"} {
		if got := answerQuery(ov, serversResponse{}, q, now, defaultAliases, ""); got != want {
			t.Fatalf("%q:\n got %q\nwant %q", q, got, want)
		}
	}
	// Filijala = Filijala: skraćenica ista kao naziv se ne ponavlja.
	ov2 := netdevResponse{Locations: []*locSummary{{Name: "Filijala", Status: stOK, Total: 1, Up: 1}}}
	if got := answerQuery(ov2, serversResponse{}, "lokacija lista", now, defaultAliases, ""); got != `LOKACIJE 10:42 (1):\n1 Filijala (FI)\nIzvestaj: lokacija <broj>` {
		t.Fatal(got)
	}
	if got := answerQuery(ov, serversResponse{}, "lokacija 2", now, defaultAliases, ""); got !=
		"LOKACIJA Sever 10:42: radi preko REZERVNE veze (MPLS ne radi); MPLS ruter offline od 09:12 (2/3 na vezi)" {
		t.Fatal(got)
	}
	if got := answerQuery(ov, serversResponse{}, "lokacija 9", now, defaultAliases, ""); got != "LOKACIJA: nema lokacije broj 9 (lokacija: 5). Spisak: lokacija lista" {
		t.Fatal(got)
	}
	// Izveštaj po broju nije ograničen na dva SMS-a.
	for i := 0; i < 40; i++ {
		ov.Devices = append(ov.Devices, deviceView{NetDevice: NetDevice{Name: fmt.Sprintf("NI_SW%02d", 10+i)}, LocationName: "Dolina", Status: stDown})
	}
	if got := answerQuery(ov, serversResponse{}, "lokacija 3", now, defaultAliases, ""); len(got) <= queryReplyMax || strings.HasSuffix(got, "...") || !strings.Contains(got, "NI_SW49 offline") {
		t.Fatalf("pun izveštaj: %d %q", len(got), got)
	}
	// Po nazivu i skraćenici isti pun izveštaj kao po broju.
	full := answerQuery(ov, serversResponse{}, "lokacija 3", now, defaultAliases, "")
	for _, q := range []string{"lokacija dolina", "lokacija DO"} {
		if got := answerQuery(ov, serversResponse{}, q, now, defaultAliases, ""); got != full {
			t.Fatalf("%q: %q", q, got)
		}
	}
	// Pregled svih lokacija ostaje kratak.
	if got := answerQuery(ov, serversResponse{}, "lokacija", now, defaultAliases, ""); len(got) > queryReplyMax {
		t.Fatal("status bez lokacije")
	}
}

func TestQueryHelpAndCommands(t *testing.T) {
	ov := testOverview()
	now := time.Date(2026, 11, 10, 10, 42, 0, 0, time.Local)
	for _, q := range []string{"status", "STATUS ps", "lokacja ps", "zdravo", ""} {
		if got := answerQuery(ov, serversResponse{}, q, now, defaultAliases, ""); got != queryHelp {
			t.Errorf("%q: %q", q, got)
		}
	}
	if got := answerQuery(ov, serversResponse{}, "mpls lista", now, defaultAliases, ""); !strings.HasPrefix(got, "MPLS: izvestaj jos nije dostupan") {
		t.Fatal(got)
	}
	// Pun naziv, skraćenica i broj daju isti izveštaj.
	want := "LOKACIJA Glavni Centar 10:42: sve operativno (3/3 na vezi)"
	for _, q := range []string{"lokacija Glavni Centar", "lokacija gc", "Lokacije GC", "lokacija 1"} {
		if got := answerQuery(ov, serversResponse{}, q, now, defaultAliases, ""); got != want {
			t.Errorf("%q: %q", q, got)
		}
	}
	if len(queryHelp) > 2*160 {
		t.Fatalf("pomoć je predugačka: %d", len(queryHelp))
	}
}
