package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// Mrežna oprema (switch-evi, ruteri, Wi-Fi kontroleri...) koju konzola sama
// proverava pingom i, po želji, čita preko SNMP-a. Za razliku od računara, na
// uređaje se ništa ne instalira.

// NetDevice je jedan uređaj mrežne opreme.
type NetDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Type     string `json:"type"`               // vidi deviceTypes
	Location string `json:"location,omitempty"` // prazno = lokacija po IP adresi
	Place    string `json:"place,omitempty"`    // mesto na lokaciji (prostorija, čvorište...)
	// Role: "" = običan uređaj; "wan" = glavna (MPLS) veza lokacije (npr. WAN
	// adresa rutera operatera); "backup" = rezervna veza lokacije.
	Role   string `json:"role,omitempty"`
	Active bool   `json:"active"` // neaktivni (ugašeni, rezervni) se ne proveravaju
	SNMP   bool   `json:"snmp"`   // čitati SNMP-om (portovi, saobraćaj)
	Notify bool   `json:"notify"` // obaveštenja o padu
	Note   string `json:"note,omitempty"`
	// Uplink: naziv porta ka ostatku mreže (npr. Te1/1/1) čiji se saobraćaj
	// prikazuje kao saobraćaj uređaja; prazno = automatski izbor.
	Uplink string `json:"uplink,omitempty"`
	// SNMPProfile: dodatni SNMP nalog (naziv iz SNMP podešavanja) za uređaj
	// sa drugim podacima za prijavu; prazno = podrazumevani nalog.
	SNMPProfile string `json:"snmp_profile,omitempty"`
	// MPLS (CE ruter operatera): MPLSLink "primary" (optički) ili
	// "secondary" (radio-relejni) na svojoj lokaciji; takav uređaj se
	// prikazuje u kartici MPLS, ne u Mrežnoj opremi. MPLSIf je WAN interfejs
	// ("" = automatski, opis sadrži „WAN”), MPLSSpeed ugovorena brzina (Mb/s).
	MPLSLink  string `json:"mpls_link,omitempty"`
	MPLSIf    string `json:"mpls_if,omitempty"`
	MPLSSpeed int    `json:"mpls_speed_mbps,omitempty"`
	MPLSKind  string `json:"mpls_kind,omitempty"` // vrsta linka: optika, rr (samo prikaz)
}

// deviceTypes su dozvoljene vrste uređaja (ključ → naziv za prikaz).
var deviceTypes = map[string]string{
	"switch": "Switch", "router": "Ruter", "firewall": "Firewall", "wlc": "Wi-Fi kontroler",
	"ap": "Pristupna tačka", "ups": "UPS", "server": "Server", "printer": "Štampač", "other": "Ostalo",
}

// netdevPrefix je prefiks ID-a „agenta” u rezultatima i zaključavanju za
// ping mrežnog uređaja (da se ne meša sa ID-jevima agenata).
const netdevPrefix = "netdev:"

func clean(s string, max int) string {
	s = strings.TrimSpace(cleanText(s))
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

// normalize proverava uređaj i dovodi ga u standardan oblik.
func (d *NetDevice) normalize() error {
	d.Name = clean(d.Name, 64)
	d.Location = clean(d.Location, 64)
	d.Place = clean(d.Place, 64)
	d.Note = clean(d.Note, 200)
	d.Uplink = clean(d.Uplink, 64)
	d.IP = strings.TrimSpace(d.IP)
	if d.Name == "" {
		return errors.New("naziv uređaja je obavezan")
	}
	a, err := netip.ParseAddr(d.IP)
	if err != nil || !a.Is4() {
		return fmt.Errorf("%s: neispravna IPv4 adresa %q", d.Name, d.IP)
	}
	d.IP = a.String()
	d.Type = strings.ToLower(strings.TrimSpace(d.Type))
	if d.Type == "" {
		d.Type = "switch"
	}
	if _, ok := deviceTypes[d.Type]; !ok {
		return fmt.Errorf("%s: nepoznata vrsta uređaja %q", d.Name, d.Type)
	}
	switch d.Role {
	case "", "wan", "backup":
	default:
		return fmt.Errorf("%s: nepoznata uloga %q", d.Name, d.Role)
	}
	d.MPLSIf = clean(d.MPLSIf, 64)
	switch d.MPLSLink {
	case "":
		d.MPLSIf, d.MPLSSpeed, d.MPLSKind = "", 0, ""
	case mplsPrimary, mplsSecondary:
		if d.MPLSSpeed < 0 || d.MPLSSpeed > 100000 {
			return fmt.Errorf("%s: ugovorena brzina mora biti između 0 i 100000 Mb/s", d.Name)
		}
		if _, ok := mplsKinds[d.MPLSKind]; !ok {
			return fmt.Errorf("%s: nepoznata vrsta linka %q", d.Name, d.MPLSKind)
		}
	default:
		return fmt.Errorf("%s: nepoznat MPLS link %q", d.Name, d.MPLSLink)
	}
	return nil
}

// deviceFile je spisak uređaja u data direktorijumu (netdevices.json).
type deviceFile struct {
	path string
}

func (f deviceFile) load() ([]NetDevice, error) {
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []NetDevice
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", f.path, err)
	}
	return out, nil
}

func (f deviceFile) save(list []NetDevice) error {
	if list == nil {
		list = []NetDevice{}
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}

// ---------- uvoz iz CSV-a ----------

// importResult je ishod uvoza.
type importResult struct {
	Added   int      `json:"added"`
	Updated int      `json:"updated"`
	Errors  []string `json:"errors,omitempty"`
}

var csvHeaders = map[string]string{
	"lokacija": "location", "location": "location",
	"mesto": "place", "place": "place", "prostorija": "place",
	"naziv": "name", "name": "name", "naziv sviča": "name", "naziv uređaja": "name",
	"ip": "ip", "ip adresa": "ip", "adresa": "ip",
	"tip": "type", "vrsta": "type", "type": "type",
	"uloga": "role", "role": "role",
	"aktivan": "active", "active": "active",
	"snmp":        "snmp",
	"obaveštenja": "notify", "obavestenja": "notify", "notify": "notify",
	"napomena": "note", "note": "note",
}

var defaultCSVOrder = []string{"location", "place", "name", "ip", "type", "role", "active", "note"}

var typeAliases = map[string]string{
	"sw": "switch", "svič": "switch", "svic": "switch", "ruter": "router", "wifi kontroler": "wlc",
	"wi-fi kontroler": "wlc", "pristupna tačka": "ap", "štampač": "printer", "stampac": "printer", "ostalo": "other",
}

var roleAliases = map[string]string{
	"": "", "-": "", "wan": "wan", "mpls": "wan", "glavna": "wan", "backup": "backup", "rezerva": "backup", "rezervna": "backup",
}

func parseBool(s string, def bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return def, nil
	case "da", "1", "true", "yes", "x", "aktivan":
		return true, nil
	case "ne", "0", "false", "no", "neaktivan", "ugašen", "ugasen", "isključen", "iskljucen":
		return false, nil
	}
	return def, fmt.Errorf("očekuje se da/ne, a ne %q", s)
}

// parseDeviceCSV čita uređaje iz teksta (CSV sa ; , ili tabom). Prvi red može
// biti zaglavlje; bez njega važi redosled defaultCSVOrder.
func parseDeviceCSV(text string) ([]NetDevice, []string) {
	text = strings.TrimPrefix(text, "\ufeff")
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	sep := ","
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if strings.Contains(l, ";") {
			sep = ";"
		} else if strings.Contains(l, "\t") {
			sep = "\t"
		}
		break
	}
	order := defaultCSVOrder
	var out []NetDevice
	var errs []string
	header := true
	for i, l := range lines {
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		fields := strings.Split(l, sep)
		for j := range fields {
			fields[j] = strings.Trim(strings.TrimSpace(fields[j]), `"`)
		}
		if header {
			header = false
			var cols []string
			known := 0
			for _, f := range fields {
				c := csvHeaders[strings.ToLower(f)]
				if c != "" {
					known++
				}
				cols = append(cols, c)
			}
			if known >= 2 {
				order = cols
				continue
			}
		}
		d := NetDevice{Active: true, SNMP: true, Notify: true}
		var rowErr error
		for j, f := range fields {
			if j >= len(order) || order[j] == "" {
				continue
			}
			switch order[j] {
			case "location":
				d.Location = f
			case "place":
				d.Place = f
			case "name":
				d.Name = f
			case "ip":
				d.IP = f
			case "type":
				t := strings.ToLower(f)
				if a, ok := typeAliases[t]; ok {
					t = a
				}
				d.Type = t
			case "role":
				r, ok := roleAliases[strings.ToLower(f)]
				if !ok {
					rowErr = fmt.Errorf("nepoznata uloga %q (wan, backup ili prazno)", f)
				}
				d.Role = r
			case "active":
				d.Active, rowErr = parseBool(f, true)
			case "snmp":
				d.SNMP, rowErr = parseBool(f, true)
			case "notify":
				d.Notify, rowErr = parseBool(f, true)
			case "note":
				d.Note = f
			}
			if rowErr != nil {
				break
			}
		}
		if rowErr == nil {
			rowErr = d.normalize()
		}
		if rowErr != nil {
			errs = append(errs, fmt.Sprintf("red %d: %v", i+1, rowErr))
			continue
		}
		out = append(out, d)
	}
	return out, errs
}

// mergeDevices dodaje uvezene uređaje u spisak; uređaj sa istom IP adresom se
// ažurira (ID ostaje isti, pa se istorija ne gubi).
func mergeDevices(list []NetDevice, in []NetDevice, newID func() string) ([]NetDevice, importResult) {
	var res importResult
	list = slices.Clone(list)
	for _, d := range in {
		if i := slices.IndexFunc(list, func(x NetDevice) bool { return x.IP == d.IP }); i >= 0 {
			// CSV ne sadrži uplink, SNMP nalog ni MPLS podatke.
			d.ID, d.Uplink, d.SNMPProfile = list[i].ID, list[i].Uplink, list[i].SNMPProfile
			d.MPLSLink, d.MPLSIf, d.MPLSSpeed, d.MPLSKind = list[i].MPLSLink, list[i].MPLSIf, list[i].MPLSSpeed, list[i].MPLSKind
			list[i] = d
			res.Updated++
			continue
		}
		d.ID = newID()
		list = append(list, d)
		res.Added++
	}
	return list, res
}
