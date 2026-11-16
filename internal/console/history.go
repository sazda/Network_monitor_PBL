package console

import (
	"archive/zip"
	"errors"
	"fmt"
	"html"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Periodi grafikona i izvoz istorije u Excel (.xlsx).

// historyRanges su ponuđeni periodi; „custom” koristi from i to (unix).
var historyRanges = map[string]time.Duration{
	"15m": 15 * time.Minute, "1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour,
	"7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
}

// seriesMPLS: saobraćaj WAN interfejsa CE rutera (isti raspored kao port).
const seriesMPLS = "mpls"

const (
	histRawDays = 7  // sva merenja
	histAggDays = 31 // 15-minutni zbirovi
)

// historyWindow čita period iz upita. Sva merenja se prikazuju za period do
// 24 h koji je ceo u poslednjih histRawDays dana; inače 15-minutni zbirovi.
func historyWindow(q url.Values, now time.Time) (from, to time.Time, agg bool, err error) {
	if q.Get("range") == "custom" {
		f, err1 := strconv.ParseInt(q.Get("from"), 10, 64)
		t, err2 := strconv.ParseInt(q.Get("to"), 10, 64)
		if err1 != nil || err2 != nil {
			return from, to, false, errors.New("neispravan proizvoljan period")
		}
		from, to = time.Unix(f, 0), time.Unix(t, 0)
		if to.After(now) {
			to = now
		}
		oldest := now.AddDate(0, 0, -histAggDays)
		switch {
		case !from.Before(to) || to.Sub(from) < time.Minute:
			return from, to, false, errors.New("period mora da traje bar 1 minut (početak pre kraja)")
		case !to.After(oldest):
			return from, to, false, fmt.Errorf("istorija se čuva %d dana – nema podataka za izabrani period", histAggDays)
		}
		if from.Before(oldest) {
			from = oldest
		}
	} else {
		span, ok := historyRanges[q.Get("range")]
		if !ok {
			span = 24 * time.Hour
		}
		to = now
		from = to.Add(-span)
	}
	agg = to.Sub(from) > 24*time.Hour || from.Before(now.AddDate(0, 0, -histRawDays).Add(time.Hour))
	return from, to, agg, nil
}

// histCol je jedna kolona izvoza: indeks vrednosti, naziv i delilac
// (npr. b/s → Mb/s).
type histCol struct {
	idx   int
	name  string
	scale float64
	dec   int
}

func historyColumns(kind string, agg bool) []histCol {
	mb := 1e6
	switch {
	case kind == seriesRTT && !agg:
		return []histCol{{0, "Odziv (ms)", 1, 2}}
	case kind == seriesRTT:
		return []histCol{{0, "Prosečan odziv (ms)", 1, 2}, {1, "Najveći odziv (ms)", 1, 2}, {2, "Gubitak (%)", 1, 1}}
	case kind == seriesSrv && !agg:
		return []histCol{{0, "CPU (%)", 1, 1}, {1, "RAM (%)", 1, 1}}
	case kind == seriesSrv:
		return []histCol{{0, "CPU prosek (%)", 1, 1}, {1, "RAM prosek (%)", 1, 1}, {2, "CPU maks (%)", 1, 1}, {3, "RAM maks (%)", 1, 1}}
	case kind == seriesTemp && !agg:
		return []histCol{{0, "Temperatura (°C)", 1, 1}}
	case kind == seriesTemp:
		return []histCol{{0, "Temperatura prosek (°C)", 1, 1}, {1, "Temperatura maks (°C)", 1, 1}}
	case kind == seriesMPLS && !agg:
		return []histCol{{0, "Ka lokaciji (Mb/s)", mb, 3}, {1, "Sa lokacije (Mb/s)", mb, 3}}
	case kind == seriesMPLS:
		return []histCol{{0, "Ka lokaciji prosek (Mb/s)", mb, 3}, {1, "Sa lokacije prosek (Mb/s)", mb, 3},
			{2, "Ka lokaciji maks (Mb/s)", mb, 3}, {3, "Sa lokacije maks (Mb/s)", mb, 3}}
	case !agg:
		return []histCol{{0, "Primljeno (Mb/s)", mb, 3}, {1, "Poslato (Mb/s)", mb, 3}, {2, "Greške primljeno", 1, 0},
			{3, "Greške poslato", 1, 0}, {4, "Odbačeni primljeno", 1, 0}, {5, "Odbačeni poslato", 1, 0}}
	}
	return []histCol{{0, "Primljeno prosek (Mb/s)", mb, 3}, {1, "Poslato prosek (Mb/s)", mb, 3}, {2, "Primljeno maks (Mb/s)", mb, 3},
		{3, "Poslato maks (Mb/s)", mb, 3}, {4, "Greške primljeno", 1, 0}, {5, "Greške poslato", 1, 0},
		{6, "Odbačeni primljeno", 1, 0}, {7, "Odbačeni poslato", 1, 0}}
}

// historyExport šalje istoriju kao .xlsx (jedan list: naslov, period,
// zaglavlje, redovi).
func historyExport(w http.ResponseWriter, name, label, kind string, from, to time.Time, agg bool, pts []tsPoint) {
	cols := historyColumns(kind, agg)
	detail := "sva merenja"
	if agg {
		detail = "15-minutni proseci i maksimumi"
	}
	title := name
	if label != "" {
		title += " – " + label
	}
	rows := [][]xcell{
		{{s: title, bold: true}},
		{{s: fmt.Sprintf("Period: %s – %s (%s)", from.Local().Format("02.01.2006. 15:04"), to.Local().Format("02.01.2006. 15:04"), detail)}},
		nil,
	}
	head := []xcell{{s: "Vreme", bold: true}}
	for _, c := range cols {
		head = append(head, xcell{s: c.name, bold: true})
	}
	rows = append(rows, head)
	for _, p := range pts {
		r := []xcell{{t: p.T, date: true}}
		for _, c := range cols {
			v := math.NaN()
			if c.idx < len(p.V) {
				v = p.V[c.idx]
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				r = append(r, xcell{})
				continue
			}
			k := math.Pow(10, float64(c.dec))
			r = append(r, xcell{n: math.Round(v/c.scale*k) / k, num: true})
		}
		rows = append(rows, r)
	}
	file := fmt.Sprintf("%s-%s-%s.xlsx", safeFileName(name), safeFileName(strings.ToLower(label)), from.Local().Format("20060102-1504"))
	if label == "" {
		file = fmt.Sprintf("%s-%s.xlsx", safeFileName(name), from.Local().Format("20060102-1504"))
	}
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	w.Header().Set("Cache-Control", "no-store")
	writeXLSX(w, "Istorija", rows, len(cols)+1)
}

// ---------- minimalan .xlsx (bez spoljnih biblioteka) ----------

type xcell struct {
	s    string
	n    float64
	t    int64 // unix, kada je date
	num  bool
	date bool
	bold bool
}

// excelTime: Excel serijski datum u lokalnom vremenu. Dodaje se pola
// milisekunde: deo dana nije tačno predstavljiv, a programi koji odsecaju
// sekunde bi 08:00:00 prikazali kao 07:59:59.
func excelTime(unix int64) float64 {
	t := time.Unix(unix, 0).Local()
	_, off := t.Zone()
	return (float64(unix+int64(off))+0.0005)/86400 + 25569
}

func colName(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}

func writeXLSX(w http.ResponseWriter, sheet string, rows [][]xcell, ncols int) {
	zw := zip.NewWriter(w)
	defer zw.Close()
	put := func(name, body string) {
		f, err := zw.Create(name)
		if err == nil {
			f.Write([]byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" + body))
		}
	}
	put("[Content_Types].xml", `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`+
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`+
		`<Default Extension="xml" ContentType="application/xml"/>`+
		`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>`+
		`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`+
		`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>`+
		`</Types>`)
	put("_rels/.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>`+
		`</Relationships>`)
	put("xl/workbook.xml", `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">`+
		`<sheets><sheet name="`+html.EscapeString(sheet)+`" sheetId="1" r:id="rId1"/></sheets></workbook>`)
	put("xl/_rels/workbook.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>`+
		`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`+
		`</Relationships>`)
	// Stilovi: 0 običan, 1 datum i vreme, 2 podebljan.
	put("xl/styles.xml", `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`+
		`<numFmts count="1"><numFmt numFmtId="164" formatCode="dd.mm.yyyy hh:mm:ss"/></numFmts>`+
		`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>`+
		`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>`+
		`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>`+
		`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>`+
		`<cellXfs count="3"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>`+
		`<xf numFmtId="164" fontId="0" fillId="0" borderId="0" xfId="0" applyNumberFormat="1"/>`+
		`<xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs>`+
		`</styleSheet>`)
	f, err := zw.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><cols>` +
		`<col min="1" max="1" width="21" customWidth="1"/>`)
	if ncols > 1 {
		fmt.Fprintf(&b, `<col min="2" max="%d" width="22" customWidth="1"/>`, ncols)
	}
	b.WriteString(`</cols><sheetData>`)
	for i, row := range rows {
		fmt.Fprintf(&b, `<row r="%d">`, i+1)
		for j, c := range row {
			ref := colName(j) + strconv.Itoa(i+1)
			switch {
			case c.date:
				fmt.Fprintf(&b, `<c r="%s" s="1"><v>%s</v></c>`, ref, strconv.FormatFloat(excelTime(c.t), 'f', 10, 64))
			case c.num:
				fmt.Fprintf(&b, `<c r="%s"><v>%s</v></c>`, ref, strconv.FormatFloat(c.n, 'f', -1, 64))
			case c.s != "":
				st := ""
				if c.bold {
					st = ` s="2"`
				}
				fmt.Fprintf(&b, `<c r="%s" t="inlineStr"%s><is><t xml:space="preserve">%s</t></is></c>`, ref, st, html.EscapeString(c.s))
			}
		}
		b.WriteString(`</row>`)
		if b.Len() > 1<<20 {
			f.Write([]byte(b.String()))
			b.Reset()
		}
	}
	b.WriteString(`</sheetData></worksheet>`)
	f.Write([]byte(b.String()))
}
