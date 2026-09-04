package cover

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
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

func TestStoreCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "covers")

	path, err := Store(dir, "hash", solidPNG(t, 20, 30))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat stored cover: %v", err)
	}
}

func TestStoreRemovesTemporaryFile(t *testing.T) {
	dir := t.TempDir()

	path, err := Store(dir, "hash", solidPNG(t, 20, 30))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || filepath.Join(dir, entries[0].Name()) != path {
		t.Errorf("cover directory entries = %v, want only %s", entries, path)
	}
}

func TestStoreReplacesExistingCover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hash.jpg")
	if err := os.WriteFile(path, []byte("partial"), 0o644); err != nil {
		t.Fatalf("write existing cover: %v", err)
	}

	storedPath, err := Store(dir, "hash", solidPNG(t, 80, 100))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if storedPath != path {
		t.Errorf("stored path = %q, want %q", storedPath, path)
	}
	if width, height := decodedSize(t, path); width != 80 || height != 100 {
		t.Errorf("stored size = %dx%d, want 80x100", width, height)
	}
}

// A byte cap on the input is not a bound on the decode: width x height x 4
// bytes of RGBA comes out of a small, highly compressible file, so a cover
// fetched from a metadata provider could otherwise make this allocate
// gigabytes. The header is read on its own first so an oversized image is
// refused before any pixel buffer exists.
func TestStoreRefusesAnImageOverThePixelLimit(t *testing.T) {
	// A PNG header claiming far more pixels than maxPixels, with no image
	// data behind it: DecodeConfig reads only the header, which is the
	// point — this must be refused without ever decoding.
	var buf bytes.Buffer
	buf.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 0, 25)
	ihdr = append(ihdr, 0, 0, 0, 13, 'I', 'H', 'D', 'R')
	ihdr = binary.BigEndian.AppendUint32(ihdr, 40000) // width
	ihdr = binary.BigEndian.AppendUint32(ihdr, 40000) // height
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	ihdr = binary.BigEndian.AppendUint32(ihdr, crc32.ChecksumIEEE(ihdr[4:]))
	buf.Write(ihdr)

	_, err := Store(t.TempDir(), "huge", buf.Bytes())
	if err == nil {
		t.Fatal("Store: want an error for a 40000x40000 image")
	}
	if !strings.Contains(err.Error(), "pixel limit") {
		t.Errorf("error = %v, want it to name the pixel limit", err)
	}
}
