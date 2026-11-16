package console

import (
	"archive/zip"
	"bytes"
	"io"
	"math"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// xlsxSheet vraća XML prvog lista (i proverava da je fajl ispravan ZIP).
func xlsxSheet(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	need := map[string]bool{"[Content_Types].xml": false, "xl/workbook.xml": false, "xl/styles.xml": false, "_rels/.rels": false}
	sheet := ""
	for _, f := range zr.File {
		need[f.Name] = true
		if f.Name == "xl/worksheets/sheet1.xml" {
			rc, _ := f.Open()
			x, _ := io.ReadAll(rc)
			rc.Close()
			sheet = string(x)
		}
	}
	for n, ok := range need {
		if !ok {
			t.Fatalf("nedostaje %s", n)
		}
	}
	return sheet
}

func TestHistoryWindow(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.Local)
	q := func(s string) url.Values { v, _ := url.ParseQuery(s); return v }
	cases := []struct {
		q    string
		span time.Duration
		agg  bool
	}{
		{"range=15m", 15 * time.Minute, false},
		{"range=24h", 24 * time.Hour, false},
		{"range=7d", 7 * 24 * time.Hour, true},
		{"", 24 * time.Hour, false},
	}
	for _, c := range cases {
		f, to, agg, err := historyWindow(q(c.q), now)
		if err != nil || to.Sub(f) != c.span || agg != c.agg || !to.Equal(now) {
			t.Errorf("%q: %v %v %v %v", c.q, to.Sub(f), agg, err, to)
		}
	}
	u := func(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }
	// Proizvoljan period: 2 h pre 3 dana – sva merenja (čuvaju se 7 dana).
	f0 := now.Add(-72 * time.Hour)
	f, to, agg, err := historyWindow(q("range=custom&from="+u(f0)+"&to="+u(f0.Add(2*time.Hour))), now)
	if err != nil || agg || !f.Equal(f0) || to.Sub(f) != 2*time.Hour {
		t.Fatalf("proizvoljan: %v %v %v %v", f, to, agg, err)
	}
	// Pre 10 dana: samo 15-minutni zbirovi.
	f0 = now.AddDate(0, 0, -10)
	if _, _, agg, err := historyWindow(q("range=custom&from="+u(f0)+"&to="+u(f0.Add(time.Hour))), now); err != nil || !agg {
		t.Fatalf("stariji od 7 dana: %v %v", agg, err)
	}
	// Kraj u budućnosti se skraćuje; početak stariji od 31 dan se pomera.
	f, to, _, err = historyWindow(q("range=custom&from="+u(now.AddDate(0, 0, -40))+"&to="+u(now.Add(time.Hour))), now)
	if err != nil || !to.Equal(now) || !f.Equal(now.AddDate(0, 0, -histAggDays)) {
		t.Fatalf("granice: %v %v %v", f, to, err)
	}
	for _, bad := range []string{
		"range=custom&from=x&to=1",
		"range=custom&from=" + u(now) + "&to=" + u(now.Add(-time.Hour)),
		"range=custom&from=" + u(now) + "&to=" + u(now.Add(30*time.Second)),
		"range=custom&from=" + u(now.AddDate(0, 0, -50)) + "&to=" + u(now.AddDate(0, 0, -40)),
	} {
		if _, _, _, err := historyWindow(q(bad), now); err == nil {
			t.Errorf("%q: prihvaćeno", bad)
		}
	}
}

func TestHistoryExportXLSX(t *testing.T) {
	w := httptest.NewRecorder()
	from := time.Date(2026, 11, 13, 8, 0, 0, 0, time.Local)
	pts := []tsPoint{
		{T: from.Unix(), V: []float64{12_345_678, 2_000_000, 1, 0, 0, 3, 0, 0}},
		{T: from.Add(15 * time.Minute).Unix(), V: []float64{math.NaN(), 1_500_000, 0, 0, 0, 0, 0, 0}},
	}
	historyExport(w, "HQ_FIN_S-I", "Gi1/0/3 <test>", seriesIf, from, from.Add(time.Hour), true, pts)
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `HQ_FIN_S-I-gi1_0_3_test-20261113-0800.xlsx`) {
		t.Fatalf("naziv: %q", cd)
	}
	sheet := xlsxSheet(t, w.Body.Bytes())
	for _, want := range []string{
		"HQ_FIN_S-I – Gi1/0/3 &lt;test&gt;", "15-minutni proseci", "Primljeno prosek (Mb/s)", "Odbačeni poslato",
		`<v>12.346</v>`, `<v>2</v>`, `r="A5" s="1"`, `<c r="B6"`, // B6 bez vrednosti se ne piše
	} {
		if want == `<c r="B6"` {
			if strings.Contains(sheet, want) {
				t.Errorf("prazna ćelija je upisana")
			}
			continue
		}
		if !strings.Contains(sheet, want) {
			t.Errorf("nema %q", want)
		}
	}
	// Excel datum: 13.11.2026. 08:00 lokalno.
	if v := excelTime(from.Unix()); v < 46339.33 || v > 46339.34 {
		t.Errorf("excel datum: %v", v)
	}
	if colName(0) != "A" || colName(25) != "Z" || colName(26) != "AA" {
		t.Error("nazivi kolona")
	}
}
