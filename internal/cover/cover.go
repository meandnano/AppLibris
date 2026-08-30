// Package cover turns a raw embedded cover image into the stored thumbnail
// DESIGN.md describes: resized to ~400px on the long edge, JPEG, written to
// a derived directory keyed by content hash.
package cover

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
)

const (
	maxLongEdge = 400
	jpegQuality = 85
)

// Store decodes raw, resizes it so its long edge is ~maxLongEdge (never
// upscaling a smaller source), and writes it as a JPEG to
// dir/contentHash.jpg. dir must already exist — Store doesn't create it.
func Store(dir, contentHash string, raw []byte) (string, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode cover image: %w", err)
	}

	path := filepath.Join(dir, contentHash+".jpg")
	f, err := os.Create(path)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	if err := jpeg.Encode(f, resize(src), &jpeg.Options{Quality: jpegQuality}); err != nil {
		return "", fmt.Errorf("encode jpeg: %w", err)
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
