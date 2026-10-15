// mkicon draws the FlexTCI icon and writes build/appicon.png (1024 px) and
// build/windows/icon.ico. From the repository root:
//
//	go -C tools/mkicon run . ../..
//
// It is its own module so the font package does not become a dependency of
// the app.
package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// A teal gradient, so FlexTCI is easy to tell from SliceTrigger's blue icon
// in the taskbar.
var (
	bgTop    = color.NRGBA{0x14, 0x8A, 0x84, 0xFF}
	bgBottom = color.NRGBA{0x08, 0x45, 0x44, 0xFF}
	white    = color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
)

func clamp01(v float64) float64 { return math.Max(0, math.Min(1, v)) }

// roundedRectSD is the signed distance from (x, y) to a rounded square of
// side n with corner radius r (negative inside).
func roundedRectSD(x, y, n, r float64) float64 {
	cx, cy := math.Abs(x-n/2)-(n/2-r), math.Abs(y-n/2)-(n/2-r)
	return math.Hypot(math.Max(cx, 0), math.Max(cy, 0)) + math.Min(math.Max(cx, cy), 0) - r
}

func blend(dst, src color.NRGBA, a float64) color.NRGBA {
	sa := float64(src.A) / 255 * a
	da := float64(dst.A) / 255
	oa := sa + da*(1-sa)
	if oa == 0 {
		return color.NRGBA{}
	}
	mix := func(s, d uint8) uint8 { return uint8((float64(s)*sa + float64(d)*da*(1-sa)) / oa) }
	return color.NRGBA{mix(src.R, dst.R), mix(src.G, dst.G), mix(src.B, dst.B), uint8(oa * 255)}
}

// draw renders the icon at n x n. "TCI" does not fit legibly below 32 px, so
// the small sizes show "T", as the channel tiles in the window do.
func draw(n int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	fn := float64(n)
	for y := 0; y < n; y++ {
		t := float64(y) / fn
		bg := color.NRGBA{
			uint8(float64(bgTop.R)*(1-t) + float64(bgBottom.R)*t),
			uint8(float64(bgTop.G)*(1-t) + float64(bgBottom.G)*t),
			uint8(float64(bgTop.B)*(1-t) + float64(bgBottom.B)*t),
			0xFF,
		}
		for x := 0; x < n; x++ {
			if a := clamp01(0.5 - roundedRectSD(float64(x)+0.5, float64(y)+0.5, fn, fn*0.2)); a > 0 {
				img.SetNRGBA(x, y, blend(img.NRGBAAt(x, y), bg, a))
			}
		}
	}

	text, size := "TCI", 0.42
	if n < 32 {
		text, size = "T", 0.72
	}
	f, err := opentype.Parse(gobold.TTF)
	if err != nil {
		log.Fatal(err)
	}
	face, err := opentype.NewFace(f, &opentype.FaceOptions{Size: fn * size, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		log.Fatal(err)
	}
	defer face.Close()
	// Draw onto a mask, then composite, so the text is anti-aliased against
	// the background.
	mask := image.NewAlpha(img.Bounds())
	d := &font.Drawer{Dst: mask, Src: image.Opaque, Face: face}
	b, _ := d.BoundString(text)
	half := fixed.I(n) / 2
	d.Dot = fixed.Point26_6{X: half - (b.Min.X+b.Max.X)/2, Y: half - (b.Min.Y+b.Max.Y)/2}
	d.DrawString(text)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if a := mask.AlphaAt(x, y).A; a > 0 {
				img.SetNRGBA(x, y, blend(img.NRGBAAt(x, y), white, float64(a)/255))
			}
		}
	}

	// A thin underline under "TCI": a quiet nod to a signal on the panadapter.
	if n >= 32 {
		y0, y1 := fn*0.70, fn*0.70+math.Max(1, fn*0.035)
		x0, x1 := fn*0.24, fn*0.76
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				fx, fy := float64(x)+0.5, float64(y)+0.5
				a := clamp01(math.Min(fy-y0, y1-fy)+0.5) * clamp01(math.Min(fx-x0, x1-fx)+0.5)
				if a > 0 {
					img.SetNRGBA(x, y, blend(img.NRGBAAt(x, y), color.NRGBA{0xFF, 0xB0, 0x3B, 0xFF}, a))
				}
			}
		}
	}
	return img
}

// ico encodes the icon at the given sizes with PNG-compressed entries.
func ico(sizes []int) []byte {
	var pngs [][]byte
	for _, n := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, draw(n)); err != nil {
			log.Fatal(err)
		}
		pngs = append(pngs, buf.Bytes())
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, [3]uint16{0, 1, uint16(len(sizes))})
	offset := 6 + 16*len(sizes)
	for i, n := range sizes {
		dim := uint8(n)
		if n >= 256 {
			dim = 0
		}
		binary.Write(&out, binary.LittleEndian, struct {
			W, H, Colors, Reserved uint8
			Planes, BPP            uint16
			Size, Offset           uint32
		}{dim, dim, 0, 0, 1, 32, uint32(len(pngs[i])), uint32(offset)})
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		out.Write(p)
	}
	return out.Bytes()
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	if err := os.WriteFile(filepath.Join(root, "build/windows/icon.ico"), ico([]int{16, 20, 24, 32, 40, 48, 64, 128, 256}), 0o644); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(filepath.Join(root, "build/appicon.png"))
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, draw(1024)); err != nil {
		log.Fatal(err)
	}
}
