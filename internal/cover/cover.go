// Package cover turns a raw embedded cover image into the stored thumbnail
// docs/notes/formats.md describes: resized to ~400px on the long edge, JPEG,
// written to a derived directory keyed by content hash.
package cover

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const (
	maxLongEdge = 400
	jpegQuality = 85
)

// MaxCoverBytes bounds the raw image Store will look at, and is the cap the
// EPUB and FB2 readers apply while extracting one, so an embedded cover is
// refused before its bytes ever accumulate rather than after. It is what
// stops a zip entry that inflates to half a gigabyte, or a <binary> that
// runs to one, from being read into memory to answer "what is this book's
// title". 8 MiB is above any cover a publisher ships (2 MB is routine, 5 MB
// rare) and two orders of magnitude under the inputs that reproduced the
// problem.
//
// internal/enrich keeps a separate, smaller cap for a cover it downloads
// from a provider: that one is a network bound whose figure the Google
// Books size choice is measured against, so raising it here would silently
// change a decision made over there
const MaxCoverBytes = 8 << 20

// maxPixels bounds a source image by its declared dimensions, read from the
// header alone before any decoder allocates for the pixels. The figure is
// sized off the worst decoder, not the RGBA arithmetic that first set it:
// image/jpeg's progressive path allocates mxx*myy*h*v coefficient blocks
// of 256 bytes per component on reading the scan header, before any
// entropy data has arrived, plus the image itself — so a file of a few
// dozen bytes declaring 7000x7000 (49 MP) measured 700 MB
// with three components and 934 MB with four, where 4 bytes x pixels
// predicted 200 MB. At 16 MP the same headers measure 228 MB and 305 MB,
// survivable once, and MaxCoverBytes keeps the file that triggers it under
// 8 MiB. 4000x4000 is still larger than any cover
const maxPixels = 16 * 1000 * 1000

// ErrUnsupportedCover marks a failure the bytes themselves decide: a format
// no registered decoder reads, a corrupt image, or one past MaxCoverBytes or
// maxPixels. Storing the same bytes again fails the same way, which is what
// the caller needs to know — internal/scanner retries a store that failed on
// I/O and must not retry one that failed on the image, since re-parsing the
// book to reach the same refusal on every sweep is the loop this exists to
// end
var ErrUnsupportedCover = errors.New("cover image cannot be decoded")

// Store decodes raw, resizes it so its long edge is ~maxLongEdge (never
// upscaling a smaller source), and atomically writes it as a JPEG to
// dir/contentHash.jpg, creating dir when needed. A failure that lies in raw
// itself wraps ErrUnsupportedCover; a filesystem failure does not
func Store(dir, contentHash string, raw []byte) (string, error) {
	if len(raw) > MaxCoverBytes {
		return "", fmt.Errorf("cover image is %d bytes, over the %d byte limit: %w", len(raw), MaxCoverBytes, ErrUnsupportedCover)
	}

	// The header is read on its own first, so an image too large to hold is
	// refused before anything allocates a pixel buffer for it.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		// The format is known when the header was recognised and then
		// failed to parse, which is worth a log line saying so; an unknown
		// format has no name to give
		if format != "" {
			return "", fmt.Errorf("decode %s cover image header: %w: %w", format, err, ErrUnsupportedCover)
		}
		return "", fmt.Errorf("decode cover image header: %w: %w", err, ErrUnsupportedCover)
	}
	// The product is taken in int64: image.Config's dimensions are int, and
	// on a 32-bit build a declared 65536x65536 wraps to a value that passes
	// the very check meant to refuse it.
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return "", fmt.Errorf("%s cover image is %dx%d, over the %d pixel limit: %w", format, cfg.Width, cfg.Height, maxPixels, ErrUnsupportedCover)
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode %s cover image: %w: %w", format, err, ErrUnsupportedCover)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cover directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, contentHash+".jpg")
	tmp, err := os.CreateTemp(dir, contentHash+".jpg.tmp*")
	if err != nil {
		return "", fmt.Errorf("create temporary cover: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := jpeg.Encode(tmp, resize(src), &jpeg.Options{Quality: jpegQuality}); err != nil {
		tmp.Close()
		return "", fmt.Errorf("encode jpeg: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close temporary cover: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("replace cover %s: %w", path, err)
	}
	return path, nil
}

func resize(src image.Image) image.Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	longEdge := width
	if height > longEdge {
		longEdge = height
	}
	if longEdge <= maxLongEdge {
		return src
	}

	scale := float64(maxLongEdge) / float64(longEdge)
	dstWidth := max(1, int(float64(width)*scale))
	dstHeight := max(1, int(float64(height)*scale))

	dst := image.NewRGBA(image.Rect(0, 0, dstWidth, dstHeight))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)
	return dst
}
