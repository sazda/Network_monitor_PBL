package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// sendEmail šalje poruku preko SMTP servera (npr. interni Exchange relay).
// Ako server nudi STARTTLS, veza se šifruje i sertifikat se proverava (na
// Windows-u preko sistemskog skladišta, gde je i interni CA).
func sendEmail(ctx context.Context, cfg NotifyConfig, m notifyMessage) error {
	addr := net.JoinHostPort(cfg.EmailHost, strconv.Itoa(cfg.EmailPort))
	d := net.Dialer{Timeout: 15 * time.Second}
	tlsCfg := &tls.Config{ServerName: cfg.EmailHost, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	var err error
	if cfg.EmailPort == 465 { // SMTPS: TLS od početka
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, cfg.EmailHost)
	if err != nil {
		return err
	}
	defer c.Close()
	if h, err := os.Hostname(); err == nil {
		if err := c.Hello(h); err != nil {
			return err
		}
	}
	if cfg.EmailPort != 465 && cfg.EmailTLS != "off" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("STARTTLS: %w", err)
			}
		}
	}
	if cfg.EmailUser != "" {
		// PlainAuth odbija slanje lozinke preko nešifrovane veze.
		if err := c.Auth(smtp.PlainAuth("", cfg.EmailUser, cfg.EmailPassword, cfg.EmailHost)); err != nil {
			return fmt.Errorf("prijava: %w", err)
		}
	}
	from, _ := mail.ParseAddress(cfg.EmailFrom)
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	var to []string
	for _, a := range cfg.recipients() {
		p, err := mail.ParseAddress(a)
		if err != nil {
			return err
		}
		if err := c.Rcpt(p.Address); err != nil {
			return fmt.Errorf("primalac %s: %w", p.Address, err)
		}
		to = append(to, p.String())
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(buildEmail(from.String(), to, m)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// foldHeader prelama dugačku vrednost zaglavlja (npr. naslov sa spiskom
// servera) na razmacima, tako da redovi ne prelaze 78 znakova (RFC 5322).
// Pri čitanju se prelom uklanja, pa je tekst isti. start je dužina „Ime: ”.
func foldHeader(start int, v string) string {
	const limit = 76
	if start+len(v) <= limit {
		return v
	}
	var b strings.Builder
	line := start
	for i, w := range strings.Split(v, " ") {
		if i > 0 {
			if line+1+len(w) > limit && line > 1 {
				b.WriteString("\r\n")
				line = 0
			}
			b.WriteByte(' ')
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

func buildEmail(from string, to []string, m notifyMessage) []byte {
	var b bytes.Buffer
	id := make([]byte, 12)
	rand.Read(id)
	host := "network-monitor"
	if at := strings.LastIndex(from, "@"); at >= 0 {
		host = strings.Trim(from[at+1:], "> ")
	}
	hdr := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, foldHeader(len(k)+2, v)) }
	hdr("From", from)
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", cleanText(m.Title)))
	hdr("Date", time.Now().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+hex.EncodeToString(id)+"@"+host+">")
	hdr("MIME-Version", "1.0")
	if m.Down {
		hdr("X-Priority", "2")
		hdr("Importance", "high")
	}
	qpPart := func(text string) {
		qp := quotedprintable.NewWriter(&b)
		qp.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n")))
		qp.Close()
		b.WriteString("\r\n")
	}
	if m.HTML == "" {
		hdr("Content-Type", "text/plain; charset=utf-8")
		hdr("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		qpPart(m.Body)
		return b.Bytes()
	}
	// multipart/related: [multipart/alternative: tekst, HTML] + slike.
	rel, alt := "rel-"+hex.EncodeToString(id), "alt-"+hex.EncodeToString(id)
	hdr("Content-Type", `multipart/related; type="multipart/alternative"; boundary="`+rel+`"`)
	b.WriteString("\r\n--" + rel + "\r\n")
	b.WriteString(`Content-Type: multipart/alternative; boundary="` + alt + "\"\r\n\r\n")
	b.WriteString("--" + alt + "\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qpPart(m.Body)
	b.WriteString("--" + alt + "\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	qpPart(m.HTML)
	b.WriteString("--" + alt + "--\r\n")
	for _, im := range m.Images {
		b.WriteString("--" + rel + "\r\n")
		fmt.Fprintf(&b, "Content-Type: image/png; name=\"%s\"\r\nContent-Transfer-Encoding: base64\r\nContent-ID: <%s>\r\nContent-Disposition: inline; filename=\"%s\"\r\n\r\n", im.Name, im.CID, im.Name)
		enc := base64.StdEncoding.EncodeToString(im.Data)
		for len(enc) > 76 {
			b.WriteString(enc[:76] + "\r\n")
			enc = enc[76:]
		}
		b.WriteString(enc + "\r\n")
	}
	b.WriteString("--" + rel + "--\r\n")
	return b.Bytes()
}

// sendTeams šalje poruku na Teams preko Workflows webhook-a (Power Automate
// tok „When a Teams webhook request is received”), kao Adaptive Card.
func sendTeams(ctx context.Context, client *http.Client, url string, m notifyMessage) error {
	color := "Good"
	if m.Down {
		color = "Attention"
	}
	card := map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard",
				"version": "1.4",
				"body": []any{
					map[string]any{"type": "TextBlock", "text": cleanText(m.Title), "weight": "Bolder", "size": "Medium", "color": color, "wrap": true},
					map[string]any{"type": "TextBlock", "text": strings.ReplaceAll(m.Body, "\n", "\n\n"), "wrap": true},
				},
			},
		}},
	}
	body, _ := json.Marshal(card)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return doNotifyRequest(client, req)
}

// sendNtfy šalje poruku na ntfy temu (https://ntfy.sh ili interni server);
// na telefonu je prikazuje ntfy aplikacija.
func sendNtfy(ctx context.Context, client *http.Client, topicURL, token string, m notifyMessage) error {
	u, err := url.Parse(topicURL)
	if err != nil {
		return err
	}
	// Naslov kao parametar u adresi: zaglavlja ne podržavaju naša slova.
	q := u.Query()
	q.Set("title", cleanText(m.Title))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(m.Body))
	if err != nil {
		return err
	}
	if m.Down {
		req.Header.Set("Priority", "high")
		req.Header.Set("Tags", "warning")
	} else if m.Recovery {
		req.Header.Set("Tags", "white_check_mark")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doNotifyRequest(client, req)
}

func doNotifyRequest(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		msg := strings.TrimSpace(string(b))
		if msg != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
