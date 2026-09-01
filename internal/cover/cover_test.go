package cover

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func solidPNG(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 50, B: 50, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return buf.Bytes()
}

func decodedSize(t *testing.T, path string) (width, height int) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	img, err := jpeg.Decode(f)
	if err != nil {
		t.Fatalf("decode %s as jpeg: %v", path, err)
	}
	b := img.Bounds()
	return b.Dx(), b.Dy()
}

func TestStoreDownscales(t *testing.T) {
	dir := t.TempDir()
	raw := solidPNG(t, 800, 600)

	path, err := Store(dir, "hash-large", raw)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("path = %q, want directory %q", path, dir)
	}

	w, h := decodedSize(t, path)
	if w != 400 || h != 300 {
		t.Errorf("stored size = %dx%d, want 400x300", w, h)
	}
}

func TestStoreDoesNotUpscale(t *testing.T) {
	dir := t.TempDir()
	raw := solidPNG(t, 100, 80)

	path, err := Store(dir, "hash-small", raw)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	w, h := decodedSize(t, path)
	if w != 100 || h != 80 {
		t.Errorf("stored size = %dx%d, want unchanged 100x80", w, h)
	}
}

func TestStoreRejectsCorruptInput(t *testing.T) {
	dir := t.TempDir()

	_, err := Store(dir, "hash-corrupt", []byte("not an image"))
	if err == nil {
		t.Fatal("Store: want error for corrupt input, got nil")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("dir has %d entries, want 0 (no file should be written on decode failure)", len(entries))
	}
}
