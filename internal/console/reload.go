package console

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// Uzrok restarta switch-a i ko ga je pokrenuo.
//
//   - Uzrok: Cisco OLD-CISCO-SYSTEM-MIB whyReload (isto što i „Last reload
//     reason” u show version), čita se posle restarta.
//   - Ko: switch neposredno pre restarta šalje syslog poruku
//     „%SYS-5-RELOAD: Reload requested by <nalog> on vty0 (<adresa>)…”.
//     Konzola prima syslog (UDP, podrazumevano port 5514) samo sa adresa
//     uređaja upisanih u konzolu. Na switch-u:
//     logging host <konzola> transport udp port 5514

const (
	oidWhyReload      = ".1.3.6.1.4.1.9.2.1.2.0"
	defaultSyslogAddr = ":5514"
	reloadReqFor      = 2 * time.Hour // zahtev za restart važi za restart u ovom roku
	reloadReadUptime  = 3 * 3600      // razlog se čita dok switch radi kraće od ovoga (s)
)

// reloadRequest je poruka switch-a o zatraženom restartu (syslog).
type reloadRequest struct {
	User   string
	Line   string // vty0, console…
	From   string // adresa sa koje je korisnik bio prijavljen
	Reason string
	At     time.Time
}

// reloadReasonText prevodi Cisco razlog restarta.
func reloadReasonText(r string) string {
	r = strings.TrimSpace(strings.TrimRight(r, "."))
	l := strings.ToLower(r)
	switch {
	case r == "":
		return ""
	case strings.Contains(l, "power"):
		return "nestanak ili uključenje napajanja"
	case strings.Contains(l, "reload command") || l == "reload" || strings.Contains(l, "reload requested"):
		return "softverski restart (reload)"
	case strings.Contains(l, "image install") || strings.Contains(l, "install"):
		return "restart posle instalacije softvera"
	case strings.Contains(l, "exception") || strings.Contains(l, "crash") || strings.Contains(l, "watchdog") || strings.Contains(l, "error"):
		return "pad softvera (" + r + ")"
	}
	return r
}

var reloadMsgRe = regexp.MustCompile(`%SYS-5-RELOAD:\s*Reload requested by (.+?) on (\S+?)(?: \(([^)]*)\))?\.(?:\s*Reload Reason:\s*(.+?))?\.?\s*$`)

// parseReloadSyslog izdvaja zahtev za restart iz syslog poruke.
func parseReloadSyslog(msg string) (reloadRequest, bool) {
	m := reloadMsgRe.FindStringSubmatch(strings.TrimSpace(msg))
	if m == nil {
		return reloadRequest{}, false
	}
	return reloadRequest{User: strings.TrimSpace(m[1]), Line: m[2], From: m[3], Reason: m[4]}, true
}

// reloadSuffix: dodatak tekstu događaja „restart”. Kada je restart zatražio
// korisnik (syslog), navodi se samo ko i sa koje adrese: SNMP razlog posle
// komande reload na nekim modelima (C2960L) pogrešno glasi „power-on”.
//
// C2960L posle komande reload javlja „power-on”, isto kao posle nestanka
// struje; bez syslog poruke uzrok se zato za njega ne može znati.
func reloadSuffix(why, model string, req *reloadRequest) string {
	if req != nil && req.User != "" {
		p := " – restart, pokrenuo " + req.User
		if req.From != "" {
			p += " sa " + req.From
		} else if req.Line != "" {
			p += " (" + req.Line + ")"
		}
		return p
	}
	if strings.Contains(strings.ToLower(why), "power") && strings.Contains(model, "2960") {
		return " – uzrok nepoznat (nestanak struje ili reload)"
	}
	var parts []string
	if t := reloadReasonText(why); t != "" {
		parts = append(parts, t)
	} else if req != nil && req.Reason != "" {
		parts = append(parts, reloadReasonText(req.Reason))
	}
	if req != nil && req.User != "" {
		p := "pokrenuo " + req.User
		if req.From != "" {
			p += " sa " + req.From
		} else if req.Line != "" {
			p += " (" + req.Line + ")"
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return ""
	}
	return " – " + strings.Join(parts, ", ")
}

// readWhyReload čita razlog poslednjeg restarta (Cisco); "" ako ga uređaj ne daje.
func readWhyReload(c snmpClient) string {
	vars, err := c.Get([]string{oidWhyReload})
	if err != nil {
		return ""
	}
	for _, v := range vars {
		if v.Name == oidWhyReload {
			return strings.TrimSpace(pduString(v))
		}
	}
	return ""
}

// ---------- syslog ----------

func (m *netMonitor) syslogLoop(addr string) {
	defer m.wg.Done()
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		m.log.Printf("syslog (ko je restartovao switch): prijem na %s nije moguć: %v", addr, err)
		return
	}
	m.log.Printf("syslog: prijem poruka switch-eva na UDP %s", pc.LocalAddr())
	m.syslogServe(pc)
}

// syslogServe prima poruke dok se monitor ne zaustavi.
func (m *netMonitor) syslogServe(pc net.PacketConn) {
	go func() { <-m.stop; pc.Close() }()
	buf := make([]byte, 4096)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		ip := ""
		if ua, ok := from.(*net.UDPAddr); ok {
			ip = ua.IP.String()
		}
		m.syslogIn(ip, string(buf[:n]), m.now())
	}
}

// syslogIn obrađuje jednu syslog poruku; prihvataju se samo poruke sa
// adresa uređaja upisanih u konzolu.
func (m *netMonitor) syslogIn(ip, msg string, now time.Time) {
	req, ok := parseReloadSyslog(msg)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.devices {
		if d.IP != ip || !d.Active {
			continue
		}
		req.At = now
		m.rt(d.ID).reloadReq = &req
		who := req.User
		if req.From != "" {
			who += " sa " + req.From
		}
		m.recordLocked([]NetEvent{{Time: now, Kind: "reload_req", DeviceID: d.ID, Device: d.Name, Location: m.locationOf(d),
			Text: fmt.Sprintf("%s: restart je zatražio %s", d.Name, who)}})
		return
	}
}
