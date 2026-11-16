package console

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// Obaveštenje o padu i povratku MPLS linka: SMS (tekst) i e-mail sa
// grafikonom (PNG) saobraćaja i odziva oba linka lokacije – pri padu za
// poslednjih 20 minuta, pri povratku za ceo prekid (od 5 minuta pre pada).

const (
	mplsMailBefore = 20 * time.Minute
	mplsMailPad    = 5 * time.Minute
)

type mplsMailLink struct {
	label, name, ip, id string
	up                  *bool
	reason              string
	speed               int
}

// mplsAlertLocked priprema i (u pozadini) šalje obaveštenje za događaj ev
// na ruteru d; downSince je početak prekida (za povratak).
func (m *netMonitor) mplsAlertLocked(ev NetEvent, d NetDevice, downSince time.Time) {
	locs := m.summarizeLocationsLocked()
	loc := m.locationOf(d)
	var links []mplsMailLink
	var prim, sec *mplsLinkView
	for _, x := range m.devices {
		if x.MPLSLink == "" || m.locationOf(x) != loc {
			continue
		}
		v := m.mplsLinkViewLocked(x, locs)
		if x.MPLSLink == mplsSecondary {
			sec = v
		} else {
			prim = v
		}
		links = append(links, mplsMailLink{label: mplsLabel(x.MPLSLink), name: x.Name, ip: x.IP, id: x.ID, up: v.LinkUp, reason: v.Reason, speed: x.MPLSSpeed})
	}
	if len(links) == 2 && links[0].label != "primarni" {
		links[0], links[1] = links[1], links[0]
	}
	active := mplsActive(prim, sec)
	now := ev.Time
	from := now.Add(-mplsMailBefore)
	var mark pngMark
	if ev.Kind == "mpls_up" && !downSince.IsZero() {
		from = downSince.Add(-mplsMailPad)
		mark = pngMark{From: downSince.Unix(), To: now.Unix(), Label: "prekid"}
	} else {
		mark = pngMark{From: now.Unix(), Label: "pad"}
	}
	hist := m.hist
	go func() {
		msgHTML, img := mplsMailBody(ev, d, links, active, from, now, mark, hist)
		m.sendAlertsWith(nil, nil, []NetEvent{ev}, now, func(msg *notifyMessage) {
			msg.HTML = msgHTML
			if img != nil {
				msg.Images = []mailImage{{CID: "mplschart", Name: "mpls-grafikon.png", Data: img}}
			}
		})
	}()
}

func mplsMailBody(ev NetEvent, d NetDevice, links []mplsMailLink, active string, from, to time.Time, mark pngMark, hist *tsdb) (string, []byte) {
	var img []byte
	if hist != nil {
		agg := to.Sub(from) > 24*time.Hour
		colors := [][2]pngSeries{
			{{Color: pngBlue}, {Color: pngOrange}},
			{{Color: pngGreen}, {Color: pngPurple}},
		}
		traffic := pngPanel{Title: "Saobracaj WAN interfejsa", Unit: "Mb/s"}
		rtt := pngPanel{Title: "Odziv (ping sa konzole)", Unit: "ms"}
		for i, l := range links {
			if i > 1 {
				break
			}
			in, out := colors[i][0], colors[i][1]
			in.Label, out.Label = l.label+" ka lokaciji", l.label+" sa lokacije"
			if pts, err := hist.Query("mpls/"+l.id, from, to, agg); err == nil {
				for _, p := range pts {
					in.Pts = append(in.Pts, [2]float64{float64(p.T), p.V[0] / 1e6})
					out.Pts = append(out.Pts, [2]float64{float64(p.T), p.V[1] / 1e6})
				}
			}
			traffic.Series = append(traffic.Series, in, out)
			r := pngSeries{Label: l.label, Color: colors[i][0].Color}
			if pts, err := hist.Query("rtt/"+l.id, from, to, agg); err == nil {
				for _, p := range pts {
					r.Pts = append(r.Pts, [2]float64{float64(p.T), p.V[0]})
				}
			}
			rtt.Series = append(rtt.Series, r)
		}
		img = renderChartPNG(from.Unix(), to.Unix(), []pngPanel{traffic, rtt}, []pngMark{mark})
	}
	down := ev.Kind == "mpls_down"
	color := "#16a34a"
	if down {
		color = "#dc2626"
	}
	esc := html.EscapeString
	var b strings.Builder
	fmt.Fprintf(&b, `<div style="font-family:Segoe UI,Arial,sans-serif;font-size:14px;color:#1e293b">`)
	fmt.Fprintf(&b, `<h2 style="color:%s;margin:0 0 8px">%s</h2>`, color, esc(ev.Text))
	b.WriteString(`<table style="border-collapse:collapse;margin:8px 0 12px">`)
	row := func(k, v string) {
		fmt.Fprintf(&b, `<tr><td style="padding:3px 12px 3px 0;color:#64748b">%s</td><td style="padding:3px 0"><b>%s</b></td></tr>`, esc(k), esc(v))
	}
	row("Lokacija", ev.Location)
	row("Vreme", ev.Time.Local().Format("02.01.2006. 15:04:05"))
	if !down && ev.DurationSec > 0 {
		row("Trajanje prekida", fmtDuration(time.Duration(ev.DurationSec)*time.Second))
	}
	for _, l := range links {
		st := "nepoznato"
		if l.up != nil && *l.up {
			st = "radi"
		} else if l.up != nil {
			st = "NE RADI"
			if l.reason != "" {
				st += " – " + l.reason
			}
		}
		name := fmt.Sprintf("%s (%s)", l.name, l.ip)
		if l.speed > 0 {
			name += fmt.Sprintf(", %d Mb/s", l.speed)
		}
		row(strings.ToUpper(l.label[:1])+l.label[1:]+" link", name+": "+st)
	}
	switch active {
	case mplsPrimary:
		row("Lokacija radi preko", "primarnog linka")
	case mplsSecondary:
		row("Lokacija radi preko", "sekundarnog (rezervnog) linka")
	default:
		row("Lokacija radi preko", "– (nijedan link ne radi ili nije poznato)")
	}
	b.WriteString(`</table>`)
	if img != nil {
		what := "poslednjih 20 minuta"
		if !down {
			what = "ceo prekid, od 5 minuta pre pada"
		}
		fmt.Fprintf(&b, `<p style="margin:12px 0 4px"><b>Grafikon</b> – %s (merenja na 30 s; crvena linija i senka: pad, odnosno prekid):</p>`, what)
		b.WriteString(`<img src="cid:mplschart" width="860" alt="Grafikon saobraćaja i odziva MPLS linkova" style="max-width:100%;border:1px solid #e2e8f0">`)
	}
	fmt.Fprintf(&b, `<p style="color:#64748b;font-size:12px;margin-top:12px">Network Monitor · %s</p></div>`, esc(d.Name))
	return b.String(), img
}
