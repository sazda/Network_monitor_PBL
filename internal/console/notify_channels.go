package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Kanali obaveštenja: svaki ima svoj prekidač i bira za šta se koristi
// (računari i/ili mrežna oprema). Glavni prekidač (NotifyConfig.Enabled)
// važi za sve kanale.
const (
	chEmail = "email"
	chSMS   = "sms"
	chTeams = "teams"
	chNtfy  = "ntfy"

	scopeAgents = "agents" // računari (agenti)
	scopeNet    = "net"    // mrežna oprema i lokacije
)

var channelKeys = []string{chEmail, chSMS, chTeams, chNtfy}

var channelLabels = map[string]string{chEmail: "e-mail", chSMS: "SMS", chTeams: "Teams", chNtfy: "ntfy"}

// ChannelOpts su podešavanja jednog kanala.
type ChannelOpts struct {
	On     bool `json:"on"`
	Agents bool `json:"agents"` // obaveštenja o računarima
	Net    bool `json:"net"`    // obaveštenja o mrežnoj opremi i lokacijama
	// Servers: obaveštenja o serverima (agent za servere). nil = isto kao Net
	// (podešavanja iz verzije bez servera).
	Servers *bool `json:"servers,omitempty"`
}

func (o ChannelOpts) servers() bool {
	if o.Servers == nil {
		return o.Net
	}
	return *o.Servers
}

// NotifyChannels su podešavanja svih kanala.
type NotifyChannels struct {
	Email ChannelOpts `json:"email"`
	SMS   ChannelOpts `json:"sms"`
	Teams ChannelOpts `json:"teams"`
	Ntfy  ChannelOpts `json:"ntfy"`
}

func (n *NotifyChannels) get(key string) ChannelOpts {
	switch key {
	case chEmail:
		return n.Email
	case chSMS:
		return n.SMS
	case chTeams:
		return n.Teams
	case chNtfy:
		return n.Ntfy
	}
	return ChannelOpts{}
}

// normalize popunjava kanale za podešavanja iz starije verzije (bez
// prekidača po kanalu): uključen je svaki popunjen kanal, kao i do sada.
func (c *NotifyConfig) normalize() {
	if c.Channels != nil {
		return
	}
	c.Channels = &NotifyChannels{
		Email: ChannelOpts{On: c.emailEnabled() && len(c.recipients()) > 0, Agents: true, Net: true},
		SMS:   ChannelOpts{On: c.emailEnabled() && c.SMSEmail != "", Agents: false, Net: true},
		Teams: ChannelOpts{On: c.TeamsURL != "", Agents: true, Net: true},
		Ntfy:  ChannelOpts{On: c.NtfyURL != "", Agents: true, Net: true},
	}
}

// configured kaže da li su popunjeni podaci potrebni kanalu (nezavisno od
// prekidača; proba radi i za isključen kanal).
func (c *NotifyConfig) configured(key string) bool {
	switch key {
	case chEmail:
		return c.emailEnabled() && len(c.recipients()) > 0
	case chSMS:
		return c.emailEnabled() && c.SMSEmail != ""
	case chTeams:
		return c.TeamsURL != ""
	case chNtfy:
		return c.NtfyURL != ""
	}
	return false
}

// sends kaže da li kanal šalje obaveštenja zadate vrste.
func (c *NotifyConfig) sends(key, scope string) bool {
	if !c.Enabled || c.Channels == nil || !c.configured(key) {
		return false
	}
	o := c.Channels.get(key)
	if !o.On {
		return false
	}
	switch scope {
	case scopeAgents:
		return o.Agents
	case scopeNet:
		return o.Net
	case scopeServers:
		return o.servers()
	}
	return true
}

// anyFor kaže da li bar jedan kanal šalje obaveštenja zadate vrste.
func (c *NotifyConfig) anyFor(scope string) bool {
	for _, k := range channelKeys {
		if c.sends(k, scope) {
			return true
		}
	}
	return false
}

// validateChannels proverava da uključeni kanali imaju potrebne podatke.
func (c *NotifyConfig) validateChannels() error {
	ch := c.Channels
	if ch == nil {
		return nil
	}
	if (ch.Email.On || ch.SMS.On) && !c.emailEnabled() {
		return errors.New("e-mail i SMS koriste SMTP server – unesite ga")
	}
	if ch.Email.On && len(c.recipients()) == 0 {
		return errors.New("kanal e-mail je uključen: unesite bar jednu adresu primaoca")
	}
	if ch.SMS.On && c.SMSEmail == "" {
		return errors.New("kanal SMS je uključen: unesite sanduče SMS telefona")
	}
	if ch.Teams.On && c.TeamsURL == "" {
		return errors.New("kanal Teams je uključen: unesite adresu webhook-a")
	}
	if ch.Ntfy.On && c.NtfyURL == "" {
		return errors.New("kanal ntfy je uključen: unesite adresu teme")
	}
	return nil
}

// sendChannel šalje poruku na jedan kanal (bez SMS-a, koji ide posebno).
func sendChannel(ctx context.Context, cfg NotifyConfig, key string, m notifyMessage) error {
	switch key {
	case chEmail:
		return sendEmail(ctx, cfg, m)
	case chTeams:
		return sendTeams(ctx, notifyHTTPClient(cfg), cfg.TeamsURL, m)
	case chNtfy:
		return sendNtfy(ctx, notifyHTTPClient(cfg), cfg.NtfyURL, cfg.NtfyToken, m)
	}
	return fmt.Errorf("nepoznat kanal %q", key)
}

// smsFromMessage pravi SMS tekst iz običnog obaveštenja (za računare).
func smsFromMessage(m notifyMessage, at string) string {
	title := strings.TrimPrefix(m.Title, "Network Monitor: ")
	return truncSMS(smsASCII("NetMon " + at + " " + title))
}
