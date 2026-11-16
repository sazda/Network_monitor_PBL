package console

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// SMS preko e-maila: konzola šalje kratku poruku (do 160 znakova, bez naših
// slova) na sanduče koje čita Android telefon sa aplikacijom „NetMon SMS”.
// Aplikacija prihvata samo poruke sa oznakom smsTag u naslovu i šalje tekst
// kao SMS na brojeve podešene u njoj (brojevi nikad nisu u e-mailu).
const (
	smsTag    = "[NETMON-SMS]"
	smsMaxLen = 160
	// smsCheckWindow: dnevna provera se šalje ako je konzola pokrenuta u
	// roku od ovoliko posle zadatog vremena.
	smsCheckWindow = 10 * time.Minute
)

var smsReplacer = strings.NewReplacer(
	"č", "c", "ć", "c", "š", "s", "ž", "z", "đ", "dj",
	"Č", "C", "Ć", "C", "Š", "S", "Ž", "Z", "Đ", "Dj",
	"–", "-", "—", "-", "„", "\"", "”", "\"", "“", "\"", "’", "'", "…", "...", "•", "-",
)

// smsASCII pretvara tekst u obične znakove: SMS sa našim slovima ima samo 70
// znakova (UCS-2), a bez njih 160.
func smsASCII(s string) string {
	s = smsReplacer.Replace(cleanText(s))
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}

// smsLine je kratak opis jednog događaja za SMS.
func smsLine(ev NetEvent) string {
	where := ""
	if ev.Location != "" {
		where = " (" + ev.Location + ")"
	}
	switch ev.Kind {
	case "down":
		return "PAD " + ev.Device + where
	case "up":
		return "OK " + ev.Device + where + " posle " + fmtDuration(time.Duration(ev.DurationSec)*time.Second)
	case "loc_down":
		return "PAD LOKACIJE " + ev.Location + " - nijedan uredjaj ne odgovara"
	case "loc_backup":
		return ev.Location + ": MPLS ne radi, radi preko backup veze"
	case "loc_ok":
		return "OK " + strings.TrimPrefix(ev.Text, "Lokacija ")
	case "reboot":
		s := "RESTART " + ev.Device + where
		if _, why, ok := strings.Cut(ev.Text, " – "); ok {
			s += " - " + why
		}
		return s
	case "mpls_down", "mpls_up":
		return "MPLS " + ev.Text
	case "temp_high", "temp_ok":
		return "TEMP " + ev.Text + where
	}
	return ev.Text
}

// composeNetSMS sastavlja SMS za promene na mrežnoj opremi. Ako sve ne staje
// u jedan SMS, na kraju piše koliko je događaja izostavljeno.
func composeNetSMS(downs, ups, locs []NetEvent, now time.Time) string {
	var lines []string
	for _, list := range [][]NetEvent{locs, downs, ups} {
		for _, ev := range list {
			lines = append(lines, smsASCII(smsLine(ev)))
		}
	}
	if len(lines) == 0 {
		return ""
	}
	out := "NetMon " + now.Local().Format("15:04")
	for i, l := range lines {
		sep := " "
		if i > 0 {
			sep = "; "
		}
		rest := len(lines) - i - 1
		more := ""
		if rest > 0 {
			more = fmt.Sprintf("; +%d jos", rest)
		}
		if len(out)+len(sep)+len(l)+len(more) <= smsMaxLen {
			out += sep + l
			continue
		}
		if i == 0 { // ni prvi događaj ne staje: skraćuje se
			out += sep + l
			return truncSMS(out)
		}
		return truncSMS(out + fmt.Sprintf("; +%d jos", len(lines)-i))
	}
	return out
}

func truncSMS(s string) string {
	if len(s) <= smsMaxLen {
		return s
	}
	return s[:smsMaxLen-3] + "..."
}

// sendSMSEmail šalje SMS tekst e-mailom na sanduče telefona, preko istog SMTP
// servera kao obična e-mail obaveštenja.
func sendSMSEmail(ctx context.Context, cfg NotifyConfig, text string) error {
	if !cfg.emailEnabled() || cfg.SMSEmail == "" {
		return errors.New("SMS preko e-maila nije podešen")
	}
	c := cfg
	c.EmailTo = cfg.SMSEmail
	return sendEmail(ctx, c, notifyMessage{
		Title: smsTag + " " + text,
		Body:  text + "\n\n-- \nPoruka za NetMon SMS telefon (Network Monitor). Brojevi primalaca su podeseni u aplikaciji.\n",
	})
}

// smsCheckDue kaže da li je vreme za dnevnu proveru (i za koji dan).
func smsCheckDue(cfg NotifyConfig, now time.Time, lastDay string) (string, bool) {
	if !cfg.sends(chSMS, "") || cfg.SMSCheckTime == "" {
		return "", false
	}
	t, err := time.Parse("15:04", cfg.SMSCheckTime)
	if err != nil {
		return "", false
	}
	now = now.Local()
	at := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, time.Local)
	day := now.Format("2006-01-02")
	if day == lastDay || now.Before(at) || now.Sub(at) > smsCheckWindow {
		return "", false
	}
	return day, true
}

// smsCheckLoop jednom dnevno šalje kontrolnu poruku: aplikacija na nju
// odgovara jednim SMS-om administratoru („veza OK”). Ako tog SMS-a nema,
// lanac konzola → e-mail → telefon ne radi.
func (n *notifier) smsCheckLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	last := ""
	for range t.C {
		cfg := n.config()
		day, ok := smsCheckDue(cfg, n.now(), last)
		if !ok {
			continue
		}
		last = day
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		text := "PROVERA " + n.now().Local().Format("02.01.2006 15:04")
		n.deliver(ctx, cfg, notifyMessage{Channel: chSMS, Title: "Dnevna provera SMS telefona", SMS: text})
		cancel()
	}
}

func validSMSTime(s string) bool {
	_, err := time.Parse("15:04", s)
	return err == nil && len(s) == 5
}

func (c *NotifyConfig) validateSMS() error {
	if c.SMSEmail == "" {
		return nil
	}
	if _, err := mail.ParseAddress(c.SMSEmail); err != nil {
		return fmt.Errorf("adresa za SMS telefon: %w", err)
	}
	if !c.emailEnabled() {
		return errors.New("SMS preko e-maila koristi SMTP server iz dela „E-mail” – podesite ga")
	}
	if c.SMSCheckTime != "" && !validSMSTime(c.SMSCheckTime) {
		return errors.New("vreme dnevne provere mora biti u obliku SS:MM (npr. 08:00)")
	}
	return nil
}
