//go:build ignore

// icongen draws the app icons. Run it from the repo root:
//
//	go run tools/icongen.go
//
// It is a generator, not part of the build (the ignore tag keeps it out of ./...), and it
// uses nothing but the standard library — no image toolchain, no rasteriser, and the same
// source produces the same bytes on any machine.
//
// The mark: three rounded bars of decreasing width on a dark rounded square. It reads as
// a list of sessions, which is what the page is, and the shapes are large enough to
// survive being 48px in a browser tab.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const super = 4 // supersampling factor: drawn big, averaged down

// Brand colours, matching the page's --accent and its panel background
var (
	bgTop    = color.RGBA{0x1b, 0x20, 0x2b, 0xff}
	bgBottom = color.RGBA{0x12, 0x15, 0x1c, 0xff}
	barFrom  = color.RGBA{0x7a, 0xa2, 0xf7, 0xff} // --accent
	barTo    = color.RGBA{0xbb, 0x9a, 0xf7, 0xff} // violet, as the logo dot ends
)

type canvas struct {
	w, h int
	px   []color.RGBA
}

func newCanvas(w, h int) *canvas { return &canvas{w: w, h: h, px: make([]color.RGBA, w*h)} }

func (c *canvas) set(x, y int, col color.RGBA) {
	if x < 0 || y < 0 || x >= c.w || y >= c.h {
		return
	}
	c.px[y*c.w+x] = col
}

func mix(a, b color.RGBA, t float64) color.RGBA {
	lerp := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.RGBA{lerp(a.R, b.R), lerp(a.G, b.G), lerp(a.B, b.B), lerp(a.A, b.A)}
}

// coverage of a rounded rectangle at a point, sampled 1 = inside
func insideRoundedRect(px, py, x, y, w, h, r float64) bool {
	if px < x || px > x+w || py < y || py > y+h {
		return false
	}
	// corners: distance to the corner circle's centre
	cx := math.Min(math.Max(px, x+r), x+w-r)
	cy := math.Min(math.Max(py, y+r), y+h-r)
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

// drawIcon renders one size. rounded tells it whether to round the background itself:
// a maskable icon must fill the square, since the launcher crops it.
func drawIcon(size int, rounded bool, pad float64) *image.RGBA {
	w := size * super
	c := newCanvas(w, w)

	radius := 0.0
	if rounded {
		radius = float64(w) * 0.225 // the usual app-icon corner
	}
	for y := 0; y < w; y++ {
		for x := 0; x < w; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			if !insideRoundedRect(fx, fy, 0, 0, float64(w), float64(w), radius) {
				continue
			}
			// a soft vertical gradient, so a large icon does not read as a flat block
			c.set(x, y, mix(bgTop, bgBottom, fy/float64(w)))
		}
	}

	// The glyph: three bars. Left-aligned, decreasing width, with the same corner radius
	// as the background at a smaller scale.
	inner := float64(w) * pad
	bx := (float64(w) - inner) / 2
	by := (float64(w) - inner) / 2
	barH := inner * 0.17
	gap := inner * 0.115
	widths := []float64{1.0, 0.72, 0.46}
	totalH := barH*float64(len(widths)) + gap*float64(len(widths)-1)
	startY := by + (inner-totalH)/2

	for i, frac := range widths {
		y := startY + float64(i)*(barH+gap)
		barW := inner * frac
		col := mix(barFrom, barTo, float64(i)/float64(len(widths)-1))
		for py := int(y); py < int(y+barH)+1; py++ {
			for px := int(bx); px < int(bx+barW)+1; px++ {
				fx, fy := float64(px)+0.5, float64(py)+0.5
				if insideRoundedRect(fx, fy, bx, y, barW, barH, barH/2) {
					c.set(px, py, col)
				}
			}
		}
	}

	// box-filter down to the target size: all the anti-aliasing this needs
	out := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a, n uint32
			for dy := 0; dy < super; dy++ {
				for dx := 0; dx < super; dx++ {
					p := c.px[(y*super+dy)*w+(x*super+dx)]
					r += uint32(p.R) * uint32(p.A)
					g += uint32(p.G) * uint32(p.A)
					b += uint32(p.B) * uint32(p.A)
					a += uint32(p.A)
					n++
				}
			}
			if a == 0 {
				out.SetRGBA(x, y, color.RGBA{})
				continue
			}
			out.SetRGBA(x, y, color.RGBA{
				uint8(r / a), uint8(g / a), uint8(b / a),
				uint8(a / uint32(n)),
			})
		}
	}
	return out
}

func write(path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
	info, _ := f.Stat()
	println("wrote", path, int(info.Size()), "bytes")
}

// writeICO wraps one PNG in an ICO container. Modern browsers read a PNG payload
// directly (the format allows it since Vista), so the tab icon and the home-screen icon
// come from the same drawing without a second rasteriser.
func writeICO(path string, size int) {
	img := drawIcon(size, true, 0.66)
	var payload bytes.Buffer
	if err := png.Encode(&payload, img); err != nil {
		panic(err)
	}
	body := payload.Bytes()

	var out bytes.Buffer
	// ICONDIR: reserved, type 1 (icon), image count
	out.Write([]byte{0, 0, 1, 0, 1, 0})
	// ICONDIRENTRY: width, height, palette, reserved, planes, bpp, size, offset
	dims := byte(size)
	if size >= 256 {
		dims = 0 // 0 means 256 in this field
	}
	out.Write([]byte{dims, dims, 0, 0, 1, 0, 0, 0})
	binary.Write(&out, binary.LittleEndian, uint32(len(body)))
	binary.Write(&out, binary.LittleEndian, uint32(22)) // 6 + 16
	out.Write(body)

	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		panic(err)
	}
	println("wrote", path, out.Len(), "bytes")
}

func main() {
	const dir = "internal/app/ui/"
	// Any purpose: rounded, glyph at 62% — the page's own icon
	write(dir+"icon-192.png", drawIcon(192, true, 0.62))
	write(dir+"icon-512.png", drawIcon(512, true, 0.62))
	// Maskable: the launcher crops to whatever shape it likes, so the background fills the
	// square and the glyph stays inside the safe zone (the centre 80% circle)
	write(dir+"icon-maskable-512.png", drawIcon(512, false, 0.46))
	// iOS asks for one icon and uses it for the home screen
	write(dir+"apple-touch-icon.png", drawIcon(180, false, 0.58))
	// The browser tab's icon, drawn from the same source so the two agree
	writeICO(dir+"favicon.ico", 64)
}
