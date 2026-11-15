//go:build windows

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
)

// drawIcon draws the tray icon: a screen on a stand. The screen is filled
// while mirroring is active. The icon is drawn at run time, so the program
// needs no resource file.
func drawIcon(size int, active bool) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	frame := color.NRGBA{R: 0x9A, G: 0x9A, B: 0x9A, A: 0xFF}
	fill := color.NRGBA{R: 0x3A, G: 0x3A, B: 0x3A, A: 0xFF}
	if active {
		frame = color.NRGBA{R: 0x1E, G: 0x88, B: 0xE5, A: 0xFF}
		fill = color.NRGBA{R: 0x90, G: 0xCA, B: 0xF9, A: 0xFF}
	}
	unit := func(v int) int { return v * size / 32 }
	rect := func(x0, y0, x1, y1 int, c color.NRGBA) {
		for y := unit(y0); y < unit(y1); y++ {
			for x := unit(x0); x < unit(x1); x++ {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	rect(2, 4, 30, 23, frame)   // screen border
	rect(5, 7, 27, 20, fill)    // screen
	rect(14, 23, 18, 26, frame) // stand
	rect(9, 26, 23, 28, frame)  // foot
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}
