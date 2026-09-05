package ocr

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"  // registered for image.Decode
	_ "image/jpeg" // registered for image.Decode
	"image/png"
	"os"
	"sort"
)

// Preprocessing is where most of the accuracy in a Tesseract pipeline comes
// from. Tesseract binarises internally with a global threshold, which is the
// wrong tool for a phone photo: a receipt held under a ceiling light is bright
// at the top and dim at the bottom, and one global cut-off either loses the
// dim text or floods the bright text. Everything here is pure Go, so the server
// binary keeps its single-file deployment and needs no ImageMagick.

// maxPixels caps what will be decoded. A 50-megapixel image would allocate
// roughly 200MB as RGBA before anything useful happened, and the production box
// is a t3.micro with 1GB of RAM shared with SQLite.
const maxPixels = 40_000_000

// targetLongEdge is what images are scaled to before OCR, and it is the single
// most important number in this file. It was chosen by sweeping it over a set
// of deliberately hard receipt photos (noisy, unevenly lit, slightly skewed)
// and counting how many totals came out right:
//
//	target   greyscale only   with Sauvola
//	  800        0/6              3/6
//	 1100        0/6              4/6
//	 1400        0/6              1/6
//	 1800        0/6              0/6
//
// Two things fall out of that table. Downscaling is the cheapest denoiser
// available -- the box filter averages every source pixel that lands in a
// target cell, and sensor noise averages to nothing -- so a working resolution
// far below the camera's is a feature rather than a compromise. And the gain
// only materialises once the image is binarised: greyscale alone scored zero at
// every resolution tried, because Tesseract's own global threshold cannot cope
// with a page that is brighter at the top than the bottom.
//
// Anything above roughly 1400 leaves pixel noise intact and the recognition
// collapses; anything below 800 starts dissolving the small print.
const targetLongEdge = 1100

// minLongEdge is the point below which an image is upscaled instead. Tesseract
// needs roughly 30 pixels of x-height; a thumbnail gives it nothing to work with.
const minLongEdge = 1000

// ErrUnsupportedImage means the file could not be decoded by any registered
// format. The caller turns this into a needs-review outcome rather than a
// failure, because the file may still be a perfectly good receipt.
var ErrUnsupportedImage = errors.New("unsupported image format")

// prepare decodes an image file, cleans it up for OCR and writes it out as a
// PNG. PNG rather than JPEG deliberately: the output is near-binary black and
// white, where JPEG's ringing artefacts would put grey haloes around every
// letter edge.
func prepare(srcPath, dstPath string, binarise bool) error {
	return prepareWith(srcPath, dstPath, targetLongEdge, binarise)
}

// prepareWith is prepare with the working resolution given explicitly, so the
// benchmark can sweep it. The chosen default is not arbitrary: see the comment
// on targetLongEdge.
func prepareWith(srcPath, dstPath string, target int, binarise bool) error {
	img, err := decodeFile(srcPath)
	if err != nil {
		return err
	}

	gray := toGray(img)
	gray = resizeGray(gray, target, minLongEdge)
	stretchContrast(gray)
	if binarise {
		gray = sauvola(gray)
	}

	out, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create prepared image: %w", err)
	}
	defer out.Close()
	if err := png.Encode(out, gray); err != nil {
		return fmt.Errorf("encode prepared image: %w", err)
	}
	return out.Close()
}

// decodeFile reads an image, refusing one large enough to exhaust memory before
// it is decoded rather than after.
func decodeFile(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open image: %w", err)
	}
	defer f.Close()

	// DecodeConfig reads only the header, so the dimensions are known before
	// any pixel memory is committed.
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, ErrUnsupportedImage
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, ErrUnsupportedImage
	}
	if cfg.Width*cfg.Height > maxPixels {
		return nil, fmt.Errorf("image is %dx%d, larger than this server will decode",
			cfg.Width, cfg.Height)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, err
	}

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, ErrUnsupportedImage
	}
	return img, nil
}

// toGray converts to 8-bit luminance.
func toGray(src image.Image) *image.Gray {
	if g, ok := src.(*image.Gray); ok {
		return g
	}
	b := src.Bounds()
	dst := image.NewGray(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

// resizeGray scales the image so its long edge lands in range, using a box
// filter when shrinking and bilinear interpolation when growing. A box filter
// is the right choice for downscaling text: it averages every source pixel that
// falls in the target cell, where a bilinear sample would skip most of them and
// alias thin strokes away entirely.
func resizeGray(src *image.Gray, target, minEdge int) *image.Gray {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	if w == 0 || h == 0 {
		return src
	}

	long := w
	if h > long {
		long = h
	}

	scale := 1.0
	switch {
	case long > target:
		scale = float64(target) / float64(long)
	case long < minEdge:
		scale = float64(minEdge) / float64(long)
		// Upscaling invents no detail, so more than doubling only makes the
		// blur bigger and the OCR slower.
		if scale > 2 {
			scale = 2
		}
	default:
		return src
	}

	nw := int(float64(w)*scale + 0.5)
	nh := int(float64(h)*scale + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	if nw == w && nh == h {
		return src
	}

	dst := image.NewGray(image.Rect(0, 0, nw, nh))
	if scale < 1 {
		boxDownscale(src, dst)
	} else {
		bilinearUpscale(src, dst)
	}
	return dst
}

func boxDownscale(src, dst *image.Gray) {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()

	for y := 0; y < dh; y++ {
		y0 := y * sh / dh
		y1 := (y + 1) * sh / dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0 := x * sw / dw
			x1 := (x + 1) * sw / dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			sum, n := 0, 0
			for sy := y0; sy < y1; sy++ {
				row := sy * src.Stride
				for sx := x0; sx < x1; sx++ {
					sum += int(src.Pix[row+sx])
					n++
				}
			}
			dst.Pix[y*dst.Stride+x] = uint8(sum / n)
		}
	}
}

func bilinearUpscale(src, dst *image.Gray) {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()

	for y := 0; y < dh; y++ {
		fy := (float64(y) + 0.5) * float64(sh) / float64(dh)
		y0 := int(fy - 0.5)
		if y0 < 0 {
			y0 = 0
		}
		y1 := y0 + 1
		if y1 >= sh {
			y1 = sh - 1
		}
		wy := fy - 0.5 - float64(y0)

		for x := 0; x < dw; x++ {
			fx := (float64(x) + 0.5) * float64(sw) / float64(dw)
			x0 := int(fx - 0.5)
			if x0 < 0 {
				x0 = 0
			}
			x1 := x0 + 1
			if x1 >= sw {
				x1 = sw - 1
			}
			wx := fx - 0.5 - float64(x0)

			p00 := float64(src.Pix[y0*src.Stride+x0])
			p01 := float64(src.Pix[y0*src.Stride+x1])
			p10 := float64(src.Pix[y1*src.Stride+x0])
			p11 := float64(src.Pix[y1*src.Stride+x1])

			top := p00 + (p01-p00)*wx
			bot := p10 + (p11-p10)*wx
			dst.Pix[y*dst.Stride+x] = uint8(top + (bot-top)*wy + 0.5)
		}
	}
}

// stretchContrast rescales the image so the 2nd and 98th percentiles become
// black and white. Percentiles rather than the true min and max, because a
// single dust speck or a blown highlight would otherwise define the whole range
// and leave the actual text occupying a narrow band in the middle.
func stretchContrast(g *image.Gray) {
	var hist [256]int
	for _, p := range g.Pix {
		hist[p]++
	}
	total := len(g.Pix)
	if total == 0 {
		return
	}

	lo, hi := 0, 255
	cut := total * 2 / 100

	acc := 0
	for i := 0; i < 256; i++ {
		acc += hist[i]
		if acc > cut {
			lo = i
			break
		}
	}
	acc = 0
	for i := 255; i >= 0; i-- {
		acc += hist[i]
		if acc > cut {
			hi = i
			break
		}
	}
	// A flat image has nothing to stretch, and dividing by the range would
	// amplify sensor noise into a page of static.
	if hi-lo < 16 {
		return
	}

	var lut [256]uint8
	span := float64(hi - lo)
	for i := 0; i < 256; i++ {
		v := (float64(i) - float64(lo)) / span * 255
		switch {
		case v < 0:
			lut[i] = 0
		case v > 255:
			lut[i] = 255
		default:
			lut[i] = uint8(v + 0.5)
		}
	}
	for i, p := range g.Pix {
		g.Pix[i] = lut[p]
	}
}

// sauvola binarises with a threshold computed per pixel from the mean and
// standard deviation of its neighbourhood, which is what makes it survive the
// uneven lighting of a hand-held photo. Integral images make it O(1) per pixel
// regardless of window size, so a large window costs nothing extra.
func sauvola(g *image.Gray) *image.Gray {
	w, h := g.Bounds().Dx(), g.Bounds().Dy()
	if w < 3 || h < 3 {
		return g
	}

	// One row and column of padding so a window clipped at the edge still has
	// a rectangle to subtract.
	stride := w + 1
	sum := make([]int64, stride*(h+1))
	sqsum := make([]int64, stride*(h+1))

	for y := 0; y < h; y++ {
		var rowSum, rowSq int64
		for x := 0; x < w; x++ {
			v := int64(g.Pix[y*g.Stride+x])
			rowSum += v
			rowSq += v * v
			sum[(y+1)*stride+(x+1)] = sum[y*stride+(x+1)] + rowSum
			sqsum[(y+1)*stride+(x+1)] = sqsum[y*stride+(x+1)] + rowSq
		}
	}

	// The window should span a few characters. A fixed pixel count would be
	// wrong across the range of sizes reaching this function, so it is derived
	// from the image and then clamped.
	win := w / 24
	if win < 15 {
		win = 15
	}
	if win > 60 {
		win = 60
	}
	rad := win / 2

	const k = 0.34 // Sauvola's parameter; 0.2 is the classic value, but text
	// photographed on white receipt paper needs a firmer hand
	const dynamicRange = 128.0

	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0 := clampInt(y-rad, 0, h)
		y1 := clampInt(y+rad+1, 0, h)
		for x := 0; x < w; x++ {
			x0 := clampInt(x-rad, 0, w)
			x1 := clampInt(x+rad+1, 0, w)

			area := int64((y1 - y0) * (x1 - x0))
			if area <= 0 {
				continue
			}
			s := rectSum(sum, stride, x0, y0, x1, y1)
			sq := rectSum(sqsum, stride, x0, y0, x1, y1)

			mean := float64(s) / float64(area)
			variance := float64(sq)/float64(area) - mean*mean
			if variance < 0 {
				variance = 0
			}
			std := sqrt(variance)

			threshold := mean * (1 + k*(std/dynamicRange-1))
			if float64(g.Pix[y*g.Stride+x]) > threshold {
				out.Pix[y*out.Stride+x] = 255
			}
		}
	}
	return out
}

func rectSum(integral []int64, stride, x0, y0, x1, y1 int) int64 {
	return integral[y1*stride+x1] -
		integral[y0*stride+x1] -
		integral[y1*stride+x0] +
		integral[y0*stride+x0]
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// sqrt is Newton's method, used rather than math.Sqrt only to keep this file's
// import list to what it genuinely needs; the values here are small and
// well-conditioned.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 12; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// medianOf is used by the engine to summarise per-word confidences. It sorts a
// copy, because the caller's slice order is meaningful elsewhere.
func medianOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	c := append([]float64(nil), vals...)
	sort.Float64s(c)
	mid := len(c) / 2
	if len(c)%2 == 1 {
		return c[mid]
	}
	return (c[mid-1] + c[mid]) / 2
}
