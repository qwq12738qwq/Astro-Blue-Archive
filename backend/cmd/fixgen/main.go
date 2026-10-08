package main

import (
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
)

func scene(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// Smooth gradients plus a hard edge, so a lossy encoder has real work to
			// do and a size difference is measurable.
			r := uint8(120 + 100*math.Sin(float64(x)/float64(w)*math.Pi))
			g := uint8(40 + 180*float64(y)/float64(h))
			b := uint8(200 - 150*math.Cos(float64(x+y)/float64(w+h)*math.Pi))
			if x > w/3 && x < 2*w/3 && y > h/3 && y < 2*h/3 {
				r, g, b = 250, 250, 20
			}
			m.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	return m
}

func main() {
	img := scene(160, 120)

	f, _ := os.Create("../tests/fixtures/sample.jpg")
	_ = jpeg.Encode(f, img, &jpeg.Options{Quality: 92})
	_ = f.Close()

	f, _ = os.Create("../tests/fixtures/sample.png")
	_ = png.Encode(f, img)
	_ = f.Close()

	f, _ = os.Create("../tests/fixtures/sample.gif")
	_ = gif.Encode(f, img, nil)
	_ = f.Close()

	// A deliberately oversized-dimension image: tiny on disk, huge decoded. 6000x4000
	// of flat colour compresses to a few kilobytes while decoding to 24 megapixels.
	huge := image.NewRGBA(image.Rect(0, 0, 6000, 4000))
	for i := range huge.Pix {
		huge.Pix[i] = 90
	}
	f, _ = os.Create("../tests/fixtures/decompression-bomb.png")
	_ = png.Encode(f, huge)
	_ = f.Close()
}
