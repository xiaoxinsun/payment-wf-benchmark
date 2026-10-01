package report

import (
	"fmt"
	"math"
	"strings"
)

// Colour-blind-safe palette, one colour per variant everywhere.
var variantColour = map[string]string{"v1a": "#4477AA", "v1b": "#EE7733", "v2": "#228833"}

var variantName = map[string]string{"v1a": "V1a Temporal baseline", "v1b": "V1b Temporal optimised", "v2": "V2 DBOS"}

type series struct {
	Name   string
	Colour string
	Vals   []float64 // bars: one per category; lines: y values
	Xs     []float64 // lines only
}

func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if v <= m*exp {
			return m * exp
		}
	}
	return 10 * exp
}

func fmtNum(v float64) string {
	switch {
	case v >= 1000:
		return fmt.Sprintf("%.0f", v)
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

const (
	cw, ch             = 720, 340
	ml, mr, mt, mb     = 64, 20, 44, 64
	fontStack          = "system-ui, -apple-system, Segoe UI, sans-serif"
	axisColour, gridCl = "#555", "#ddd"
)

func header(title string) *strings.Builder {
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d" font-family="%s" font-size="12">`, cw, ch, cw, ch, fontStack)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="#fff"/>`)
	fmt.Fprintf(&b, `<text x="%d" y="24" font-size="15" font-weight="600" fill="#222">%s</text>`, ml, esc(title))
	return &b
}

func legend(b *strings.Builder, ss []series) {
	x := ml
	for _, s := range ss {
		fmt.Fprintf(b, `<rect x="%d" y="%d" width="12" height="12" fill="%s"/><text x="%d" y="%d" fill="#222">%s</text>`, x, ch-22, s.Colour, x+17, ch-12, esc(s.Name))
		x += 30 + 7*len(s.Name)
	}
}

// barChart draws grouped bars; log scales the y axis (for latency spanning decades).
func barChart(title, ylabel string, cats []string, ss []series, log bool) string {
	b := header(title)
	pw, ph := float64(cw-ml-mr), float64(ch-mt-mb)
	maxV := 0.0
	for _, s := range ss {
		for _, v := range s.Vals {
			maxV = math.Max(maxV, v)
		}
	}
	top := niceMax(maxV)
	yOf := func(v float64) float64 {
		if log {
			lo, hi := 1.0, math.Max(top, 10)
			v = math.Max(v, lo)
			return float64(mt) + ph - ph*math.Log10(v/lo)/math.Log10(hi/lo)
		}
		return float64(mt) + ph - ph*v/top
	}
	ticks := []float64{0, .25, .5, .75, 1}
	if log {
		ticks = nil
		for t := 1.0; t <= math.Max(top, 10); t *= 10 {
			ticks = append(ticks, t)
		}
	}
	for _, t := range ticks {
		v := t * top
		if log {
			v = t
		}
		y := yOf(v)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s"/><text x="%d" y="%.1f" text-anchor="end" fill="%s">%s</text>`, ml, y, cw-mr, y, gridCl, ml-6, y+4, axisColour, fmtNum(v))
	}
	fmt.Fprintf(b, `<text x="14" y="%d" transform="rotate(-90 14 %d)" text-anchor="middle" fill="%s">%s</text>`, mt+int(ph/2), mt+int(ph/2), axisColour, esc(ylabel))
	gw := pw / float64(max(len(cats), 1))
	bw := gw * 0.8 / float64(max(len(ss), 1))
	for ci, cat := range cats {
		gx := float64(ml) + gw*float64(ci) + gw*0.1
		for si, s := range ss {
			if ci >= len(s.Vals) {
				continue
			}
			v := s.Vals[ci]
			y := yOf(v)
			fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"><title>%s %s: %s</title></rect>`, gx+bw*float64(si), y, bw*0.92, float64(mt)+ph-y, s.Colour, esc(s.Name), esc(cat), fmtNum(v))
			fmt.Fprintf(b, `<text x="%.1f" y="%.1f" text-anchor="middle" font-size="10" fill="#222">%s</text>`, gx+bw*float64(si)+bw*0.46, y-3, fmtNum(v))
		}
		fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="middle" fill="%s">%s</text>`, gx+gw*0.4, mt+int(ph)+16, axisColour, esc(cat))
	}
	fmt.Fprintf(b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s"/>`, ml, mt+int(ph), cw-mr, mt+int(ph), axisColour)
	legend(b, ss)
	b.WriteString("</svg>")
	return b.String()
}

// lineChart draws series of (x,y) points with an optional horizontal reference line.
func lineChart(title, xlabel, ylabel string, ss []series, refY float64, refLabel string) string {
	b := header(title)
	pw, ph := float64(cw-ml-mr), float64(ch-mt-mb)
	maxX, maxY := 0.0, refY
	for _, s := range ss {
		for i := range s.Vals {
			maxX = math.Max(maxX, s.Xs[i])
			maxY = math.Max(maxY, s.Vals[i])
		}
	}
	topY, topX := niceMax(maxY), niceMax(maxX)
	xOf := func(x float64) float64 { return float64(ml) + pw*x/topX }
	yOf := func(y float64) float64 { return float64(mt) + ph - ph*y/topY }
	for _, t := range []float64{0, .25, .5, .75, 1} {
		y := yOf(t * topY)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="%s"/><text x="%d" y="%.1f" text-anchor="end" fill="%s">%s</text>`, ml, y, cw-mr, y, gridCl, ml-6, y+4, axisColour, fmtNum(t*topY))
		x := xOf(t * topX)
		fmt.Fprintf(b, `<text x="%.1f" y="%d" text-anchor="middle" fill="%s">%s</text>`, x, mt+int(ph)+16, axisColour, fmtNum(t*topX))
	}
	fmt.Fprintf(b, `<text x="%d" y="%d" text-anchor="middle" fill="%s">%s</text>`, ml+int(pw/2), mt+int(ph)+34, axisColour, esc(xlabel))
	fmt.Fprintf(b, `<text x="14" y="%d" transform="rotate(-90 14 %d)" text-anchor="middle" fill="%s">%s</text>`, mt+int(ph/2), mt+int(ph/2), axisColour, esc(ylabel))
	if refY > 0 {
		y := yOf(refY)
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%d" y2="%.1f" stroke="#cc3311" stroke-dasharray="5 4"/><text x="%d" y="%.1f" text-anchor="end" fill="#cc3311">%s</text>`, ml, y, cw-mr, y, cw-mr, y-4, esc(refLabel))
	}
	for _, s := range ss {
		var pts []string
		for i := range s.Vals {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", xOf(s.Xs[i]), yOf(s.Vals[i])))
		}
		fmt.Fprintf(b, `<polyline fill="none" stroke="%s" stroke-width="2" points="%s"/>`, s.Colour, strings.Join(pts, " "))
		if len(s.Vals) <= 40 {
			for i := range s.Vals {
				fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="3" fill="%s"><title>%s x=%s y=%s</title></circle>`, xOf(s.Xs[i]), yOf(s.Vals[i]), s.Colour, esc(s.Name), fmtNum(s.Xs[i]), fmtNum(s.Vals[i]))
			}
		}
	}
	fmt.Fprintf(b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s"/><line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s"/>`, ml, mt+int(ph), cw-mr, mt+int(ph), axisColour, ml, mt, ml, mt+int(ph), axisColour)
	legend(b, ss)
	b.WriteString("</svg>")
	return b.String()
}
