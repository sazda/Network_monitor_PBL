package console

import (
	"bytes"
	"context"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func TestMPLSAlertMail(t *testing.T) {
	devs := []NetDevice{
		{Name: "FIRMA_optika_DOL", IP: "10.18.21.2", Type: "router", Location: "Dolina", Active: true, SNMP: true, Notify: true, MPLSLink: mplsPrimary, MPLSSpeed: 30},
		{Name: "FIRMA_backup_RR_DOL", IP: "10.18.21.3", Type: "router", Location: "Dolina", Active: true, SNMP: true, Notify: true, MPLSLink: mplsSecondary, MPLSSpeed: 30},
	}
	m, sent := testMonitor(t, devs, nil)
	prim := &fakeCE{wan: "Gi0/0/0.2804", alias: "WAN-L3VPN", oper: 1, bgp: []int{6}, vrrp: []int{3}}
	sec := &fakeCE{wan: "Gi0/0/0.2750", alias: "WAN", oper: 1, bgp: []int{6}, vrrp: []int{2}}
	m.dialSNMP = func(_ context.Context, _ SNMPConfig, ip string) (snmpClient, error) {
		if ip == "10.18.21.2" {
			return prim, nil
		}
		return sec, nil
	}
	now := time.Now().Add(-30 * time.Minute)
	m.now = func() time.Time { return now }
	for _, d := range m.devices {
		m.rt(d.ID).status = stUp
	}
	cfg := SNMPConfig{Version: "v2c", Community: "x"}
	poll := func() {
		now = now.Add(30 * time.Second)
		for _, d := range m.devices {
			m.pollMPLS(context.Background(), cfg, d)
		}
		if m.hist != nil {
			m.hist.Add("rtt/FIRMA_optika_DOL", now, []float64{8})
			m.hist.Add("rtt/FIRMA_backup_RR_DOL", now, []float64{9})
		}
	}
	for i := 0; i < 10; i++ {
		prim.mu.Lock()
		prim.in += 30 * 1_000_000
		prim.out += 30 * 200_000
		prim.mu.Unlock()
		poll()
	}
	// Rezervni link pada (BGP), primarni radi: ranije bez obaveštenja.
	sec.mu.Lock()
	sec.bgp = []int{3}
	sec.mu.Unlock()
	poll()
	poll()
	msgs := waitSent(t, sent, 1)
	d := msgs[0]
	if !strings.Contains(d.Title, "MPLS Dolina: sekundarni MPLS link (FIRMA_backup_RR_DOL) ne radi") || !d.Down ||
		!strings.Contains(d.SMS, "MPLS Dolina: sekundarni MPLS link (FIRMA_backup_RR_DOL) ne radi - BGP") {
		t.Fatalf("pad: %q / %q", d.Title, d.SMS)
	}
	if !strings.Contains(d.HTML, "Lokacija radi preko") || !strings.Contains(d.HTML, "primarnog linka") ||
		!strings.Contains(d.HTML, `cid:mplschart`) || !strings.Contains(d.HTML, "NE RADI – BGP") {
		t.Fatalf("HTML: %s", d.HTML)
	}
	if m.hist != nil {
		if len(d.Images) != 1 {
			t.Fatalf("slika: %d", len(d.Images))
		}
		img, err := png.Decode(bytes.NewReader(d.Images[0].Data))
		if err != nil || img.Bounds().Dx() != pngW {
			t.Fatalf("PNG: %v", err)
		}
	}
	// Povratak: e-mail sa celim prekidom.
	sec.mu.Lock()
	sec.bgp = []int{6}
	sec.mu.Unlock()
	poll()
	msgs = waitSent(t, sent, 2)
	u := msgs[1]
	if !u.Recovery || !strings.Contains(u.Title, "ponovo radi") || !strings.Contains(u.HTML, "Trajanje prekida") || !strings.Contains(u.HTML, "ceo prekid") {
		t.Fatalf("povratak: %q %s", u.Title, u.HTML)
	}
}

func TestBuildEmailHTML(t *testing.T) {
	raw := buildEmail("Network Monitor <netmon@firma.local>", []string{"<a@firma.local>"}, notifyMessage{
		Title: "MPLS Dolina: pad", Body: "tekst", HTML: "<p>html š</p>", Down: true,
		Images: []mailImage{{CID: "mplschart", Name: "g.png", Data: bytes.Repeat([]byte{1, 2, 3}, 100)}},
	})
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/related" {
		t.Fatalf("%s %v", mt, err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	alt, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	amt, ap, _ := mime.ParseMediaType(alt.Header.Get("Content-Type"))
	if amt != "multipart/alternative" {
		t.Fatalf("alternativa: %s", amt)
	}
	ar := multipart.NewReader(alt, ap["boundary"])
	var types []string
	for {
		p, err := ar.NextPart()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(p) // quoted-printable se dekodira automatski
		types = append(types, p.Header.Get("Content-Type")+"="+string(b))
	}
	if len(types) != 2 || !strings.Contains(types[0], "text/plain") || !strings.Contains(types[1], "<p>html š</p>") {
		t.Fatalf("delovi: %q", types)
	}
	im, err := mr.NextPart()
	if err != nil || im.Header.Get("Content-ID") != "<mplschart>" {
		t.Fatalf("slika: %v %v", err, im.Header)
	}
	if _, err := mr.NextPart(); err != io.EOF {
		t.Fatalf("višak delova: %v", err)
	}
}
