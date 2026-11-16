package console

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// Grafikon kao PNG slika (za e-mail: Outlook ne prikazuje SVG ni skripte).
// Tekst je ASCII (ugrađeni font), pa se natpisi pišu bez dijakritika.

type pngSeries struct {
	Label string
	Color color.RGBA
	Pts   [][2]float64 // [unix, vrednost]; NaN = prekid linije
}

type pngPanel struct {
	Title  string
	Unit   string
	Series []pngSeries
}

// pngMark je označeni trenutak ili period (npr. pad linka).
type pngMark struct {
	From, To int64 // To == 0: samo trenutak
	Label    string
}

var (
	pngBlue   = color.RGBA{37, 99, 235, 255}
	pngOrange = color.RGBA{234, 88, 12, 255}
	pngGreen  = color.RGBA{22, 163, 74, 255}
	pngPurple = color.RGBA{147, 51, 234, 255}
	pngRed    = color.RGBA{220, 38, 38, 255}
	pngGrid   = color.RGBA{226, 232, 240, 255}
	pngAxis   = color.RGBA{100, 116, 139, 255}
	pngText   = color.RGBA{30, 41, 59, 255}
	pngShade  = color.RGBA{254, 226, 226, 255}
)

const (
	pngW      = 860
	pngPanelH = 210
	pngLeft   = 64
	pngRight  = 16
)

func pngString(img *image.RGBA, x, y int, s string, c color.Color) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: basicfont.Face7x13, Dot: fixed.P(x, y)}
	d.DrawString(smsASCII(s))
}

func pngStringW(s string) int { return len(smsASCII(s)) * 7 }

func pngHLine(img *image.RGBA, x0, x1, y int, c color.Color) {
	for x := x0; x <= x1; x++ {
		img.Set(x, y, c)
	}
}

func pngVLine(img *image.RGBA, x, y0, y1 int, c color.Color, dashed bool) {
	for y := y0; y <= y1; y++ {
		if !dashed || (y/4)%2 == 0 {
			img.Set(x, y, c)
		}
	}
}

// pngLine crta duž debljine 2 (Bresenham).
func pngLine(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	dx, dy := abs(x1-x0), -abs(y1-y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	e := dx + dy
	for {
		img.Set(x0, y0, c)
		img.Set(x0, y0+1, c)
		img.Set(x0+1, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * e
		if e2 >= dy {
			e += dy
			x0 += sx
		}
		if e2 <= dx {
			e += dx
			y0 += sy
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// niceMax zaokružuje gornju granicu ose na 1, 2 ili 5 × 10^n.
func niceMax(v float64) float64 {
	if v <= 0 || math.IsNaN(v) {
		return 1
	}
	p := math.Pow(10, math.Floor(math.Log10(v)))
	for _, k := range []float64{1, 2, 5, 10} {
		if v <= k*p {
			return k * p
		}
	}
	return 10 * p
}

func fmtAxis(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

// renderChartPNG crta panele jedan ispod drugog sa zajedničkom vremenskom osom.
func renderChartPNG(from, to int64, panels []pngPanel, marks []pngMark) []byte {
	h := len(panels)*pngPanelH + 10
	img := image.NewRGBA(image.Rect(0, 0, pngW, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	plotW := pngW - pngLeft - pngRight
	span := float64(to - from)
	if span <= 0 {
		span = 1
	}
	xOf := func(t float64) int { return pngLeft + int(math.Round((t-float64(from))/span*float64(plotW))) }
	// Korak vremenske ose: oko 6 oznaka.
	step := int64(60)
	for _, s := range []int64{60, 120, 300, 600, 900, 1800, 3600, 7200, 21600, 43200, 86400} {
		step = s
		if span/float64(s) <= 7 {
			break
		}
	}
	for i, p := range panels {
		top := i*pngPanelH + 8
		pTop, pBot := top+34, top+pngPanelH-28
		maxV := 0.0
		for _, s := range p.Series {
			for _, pt := range s.Pts {
				if !math.IsNaN(pt[1]) && pt[1] > maxV {
					maxV = pt[1]
				}
			}
		}
		maxV = niceMax(maxV * 1.05)
		yOf := func(v float64) int { return pBot - int(math.Round(v/maxV*float64(pBot-pTop))) }
		// Označeni period (senka) ispod svega.
		for _, mk := range marks {
			if mk.To > 0 {
				x0, x1 := max(xOf(float64(mk.From)), pngLeft), min(xOf(float64(mk.To)), pngW-pngRight)
				for x := x0; x <= x1; x++ {
					pngVLine(img, x, pTop, pBot, pngShade, false)
				}
			}
		}
		pngString(img, pngLeft, top+12, p.Title, pngText)
		// Legenda.
		lx := pngLeft
		for _, s := range p.Series {
			for dx := 0; dx < 14; dx++ {
				img.Set(lx+dx, top+24, s.Color)
				img.Set(lx+dx, top+25, s.Color)
			}
			pngString(img, lx+18, top+29, s.Label, pngText)
			lx += 18 + pngStringW(s.Label) + 16
		}
		// Mreža i ose.
		for k := 0; k <= 4; k++ {
			v := maxV * float64(k) / 4
			y := yOf(v)
			pngHLine(img, pngLeft, pngW-pngRight, y, pngGrid)
			lbl := fmtAxis(v)
			if k == 4 && p.Unit != "" {
				lbl += " " + p.Unit
			}
			pngString(img, pngLeft-6-pngStringW(lbl), y+4, lbl, pngAxis)
		}
		for t := (from/step + 1) * step; t < to; t += step {
			x := xOf(float64(t))
			pngVLine(img, x, pTop, pBot, pngGrid, false)
			lbl := time.Unix(t, 0).Local().Format("15:04")
			pngString(img, x-pngStringW(lbl)/2, pBot+16, lbl, pngAxis)
		}
		pngHLine(img, pngLeft, pngW-pngRight, pBot, pngAxis)
		// Linije.
		for _, s := range p.Series {
			var px, py int
			have := false
			for _, pt := range s.Pts {
				if math.IsNaN(pt[1]) {
					have = false
					continue
				}
				x, y := xOf(pt[0]), yOf(pt[1])
				if have {
					pngLine(img, px, py, x, y, s.Color)
				} else {
					img.Set(x, y, s.Color)
				}
				px, py, have = x, y, true
			}
		}
		// Trenutak pada / povratka.
		for _, mk := range marks {
			x := xOf(float64(mk.From))
			if x >= pngLeft && x <= pngW-pngRight {
				pngVLine(img, x, pTop, pBot, pngRed, true)
				if i == 0 && mk.Label != "" {
					pngString(img, min(x+4, pngW-pngRight-pngStringW(mk.Label)), pTop+12, mk.Label, pngRed)
				}
			}
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}
