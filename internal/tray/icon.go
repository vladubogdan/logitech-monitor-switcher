package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// makeIcon renders a simple monitor glyph as a 32x32 image. When filled is
// false the screen is drawn as an outline (used to indicate "device away").
// The shape is opaque black on a transparent background, which is what macOS
// wants for a template (auto light/dark) menu-bar icon.
func makeIcon(filled bool) *image.NRGBA {
	const s = 32
	img := image.NewNRGBA(image.Rect(0, 0, s, s))
	black := color.NRGBA{0, 0, 0, 255}

	set := func(x, y int) {
		if x >= 0 && x < s && y >= 0 && y < s {
			img.Set(x, y, black)
		}
	}
	fillRect := func(x0, y0, x1, y1 int) {
		for y := y0; y < y1; y++ {
			for x := x0; x < x1; x++ {
				set(x, y)
			}
		}
	}
	rectOutline := func(x0, y0, x1, y1, t int) {
		fillRect(x0, y0, x1, y0+t) // top
		fillRect(x0, y1-t, x1, y1) // bottom
		fillRect(x0, y0, x0+t, y1) // left
		fillRect(x1-t, y0, x1, y1) // right
	}

	// Screen body.
	if filled {
		fillRect(3, 4, 29, 21)
	} else {
		rectOutline(3, 4, 29, 21, 2)
	}
	// Neck + base stand.
	fillRect(14, 21, 18, 25)
	fillRect(9, 25, 23, 28)
	return img
}

func pngBytes(img image.Image) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// icoBytes wraps a PNG in a single-image .ico container (Windows tray).
func icoBytes(png []byte) []byte {
	var buf bytes.Buffer
	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // count
	// ICONDIRENTRY
	buf.WriteByte(32)                                         // width
	buf.WriteByte(32)                                         // height
	buf.WriteByte(0)                                          // palette
	buf.WriteByte(0)                                          // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))        // planes
	binary.Write(&buf, binary.LittleEndian, uint16(32))       // bpp
	binary.Write(&buf, binary.LittleEndian, uint32(len(png))) // size
	binary.Write(&buf, binary.LittleEndian, uint32(6+16))     // offset
	buf.Write(png)
	return buf.Bytes()
}

// iconPresentPNG / iconAwayPNG are the two states as PNG bytes.
func iconPresentPNG() []byte { return pngBytes(makeIcon(true)) }
func iconAwayPNG() []byte    { return pngBytes(makeIcon(false)) }
