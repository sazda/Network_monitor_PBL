package console

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Location je imenovana lokacija (kancelarija, VPN...) definisana opsezima
// IP adresa. Agent pripada lokaciji čiji opseg sadrži njegovu adresu; ako se
// opsezi preklapaju, pobeđuje najuži (npr. 10.0.128.0/24 ispred 10.0.0.0/16).
type Location struct {
	Name     string   `json:"name"`
	Prefixes []string `json:"prefixes"` // CIDR, npr. 10.18.0.0/16
}

// UnknownLocation je naziv za agente čija adresa ne pripada nijednoj lokaciji.
const UnknownLocation = ""

// parseLocPrefix prihvata CIDR („10.0.128.0/17”) ili skraćeni zapis početka
// adrese („10.18”, „10.0.128.”), koji se tumači kao opseg celih okteta
// (10.18 → 10.18.0.0/16, 10.0.128 → 10.0.128.0/24).
func parseLocPrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("neispravan opseg %q", s)
		}
		return p.Masked(), nil
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "*"), ".")
	parts := strings.Split(s, ".")
	if len(parts) < 1 || len(parts) > 4 || s == "" {
		return netip.Prefix{}, fmt.Errorf("neispravan početak adrese %q", s)
	}
	var b [4]byte
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 || n > 255 {
			return netip.Prefix{}, fmt.Errorf("neispravan početak adrese %q", s)
		}
		b[i] = byte(n)
	}
	return netip.PrefixFrom(netip.AddrFrom4(b), 8*len(parts)), nil
}

// formatLocPrefix prikazuje opseg u skraćenom obliku kad je to moguće.
func formatLocPrefix(p netip.Prefix) string {
	if p.Addr().Is4() && p.Bits()%8 == 0 && p.Bits() > 0 && p.Bits() < 32 {
		b := p.Addr().As4()
		parts := make([]string, p.Bits()/8)
		for i := range parts {
			parts[i] = strconv.Itoa(int(b[i]))
		}
		return strings.Join(parts, ".")
	}
	return p.String()
}

// ParseLocationsText čita lokacije iz teksta u obliku
//
//	Filijala = 10.11.11, 10.11.3
//	Dolina = 10.18
//	# komentar
func ParseLocationsText(text string) ([]Location, error) {
	var out []Location
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, rest, ok := strings.Cut(line, "=")
		if !ok {
			name, rest, ok = strings.Cut(line, ":")
		}
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("red %d: očekuje se „Naziv = opseg, opseg”", i+1)
		}
		if len([]rune(name)) > 64 {
			return nil, fmt.Errorf("red %d: naziv je predugačak", i+1)
		}
		if seen[strings.ToLower(name)] {
			return nil, fmt.Errorf("red %d: lokacija %q je već navedena", i+1, name)
		}
		seen[strings.ToLower(name)] = true
		loc := Location{Name: name}
		for _, f := range strings.FieldsFunc(rest, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' }) {
			p, err := parseLocPrefix(f)
			if err != nil {
				return nil, fmt.Errorf("red %d: %v", i+1, err)
			}
			loc.Prefixes = append(loc.Prefixes, p.String())
		}
		if len(loc.Prefixes) == 0 {
			return nil, fmt.Errorf("red %d: lokacija %q nema nijedan opseg", i+1, name)
		}
		out = append(out, loc)
	}
	return out, nil
}

// FormatLocationsText je obrnuto od ParseLocationsText.
func FormatLocationsText(locs []Location) string {
	var b strings.Builder
	for _, l := range locs {
		var ps []string
		for _, s := range l.Prefixes {
			if p, err := netip.ParsePrefix(s); err == nil {
				ps = append(ps, formatLocPrefix(p))
			}
		}
		fmt.Fprintf(&b, "%s = %s\n", l.Name, strings.Join(ps, ", "))
	}
	return b.String()
}

type locRule struct {
	prefix netip.Prefix
	name   string
}

// locator pronalazi lokaciju za adresu (najduži prefiks pobeđuje).
type locator []locRule

func newLocator(locs []Location) (locator, error) {
	var l locator
	for _, loc := range locs {
		if strings.TrimSpace(loc.Name) == "" {
			return nil, errors.New("lokacija bez naziva")
		}
		for _, s := range loc.Prefixes {
			p, err := parseLocPrefix(s)
			if err != nil {
				return nil, fmt.Errorf("lokacija %q: %v", loc.Name, err)
			}
			l = append(l, locRule{p, loc.Name})
		}
	}
	sort.SliceStable(l, func(i, j int) bool { return l[i].prefix.Bits() > l[j].prefix.Bits() })
	return l, nil
}

func (l locator) find(a netip.Addr) string {
	a = a.Unmap()
	for _, r := range l {
		if r.prefix.Contains(a) {
			return r.name
		}
	}
	return UnknownLocation
}

// locate određuje lokaciju agenta: prvo po adresi sa koje se povezao na
// konzolu (pouzdano i za VPN, gde je gateway obično kućni ruter), a ako ona
// ne pripada nijednoj lokaciji, po adresama njegovih interfejsa.
func (l locator) locate(a AgentInfo) string {
	if host, _, err := net.SplitHostPort(a.RemoteAddr); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			if name := l.find(ip); name != UnknownLocation {
				return name
			}
		}
	}
	for _, ifc := range a.Interfaces {
		for _, c := range ifc.Addrs {
			if p, err := netip.ParsePrefix(c); err == nil {
				if name := l.find(p.Addr()); name != UnknownLocation {
					return name
				}
			}
		}
	}
	return UnknownLocation
}

// ---------- API ----------

type locationsResponse struct {
	Locations []Location `json:"locations"`
	Text      string     `json:"text"`
}

func (s *Server) currentLocations() ([]Location, locator) {
	s.locMu.RLock()
	defer s.locMu.RUnlock()
	return s.locations, s.locator
}

func (s *Server) handleGetLocations(w http.ResponseWriter, r *http.Request) {
	locs, _ := s.currentLocations()
	if locs == nil {
		locs = []Location{}
	}
	writeJSON(w, http.StatusOK, locationsResponse{Locations: locs, Text: FormatLocationsText(locs)})
}

func (s *Server) handleSetLocations(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	locs, err := ParseLocationsText(req.Text)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	loc, err := newLocator(locs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if locs == nil {
		locs = []Location{} // namerno prazno (ne vraćaju se podrazumevane)
	}
	if err := s.persist(func(c *FileConfig) { c.Locations = locs }); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.locMu.Lock()
	s.locations, s.locator = locs, loc
	s.locMu.Unlock()
	s.log.Printf("lokacije izmenjene (%d); izmenio %q", len(locs), identityFrom(r).Name)
	writeJSON(w, http.StatusOK, locationsResponse{Locations: locs, Text: FormatLocationsText(locs)})
}
