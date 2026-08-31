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
// upscaling a smaller source), and atomically writes it as a JPEG to
// dir/contentHash.jpg, creating dir when needed
func Store(dir, contentHash string, raw []byte) (string, error) {
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("decode cover image: %w", err)
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
