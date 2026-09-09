package cover

import (
	"bytes"
	"encoding/binary"
	"errors"
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
	if !errors.Is(err, ErrUnsupportedCover) {
		t.Errorf("error = %v, want it to wrap ErrUnsupportedCover", err)
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

// pngHeader is a PNG declaring width x height with no image data behind it:
// DecodeConfig reads only the header, which is the point of the tests using
// it — an oversized declaration must be refused without ever decoding
func pngHeader(width, height uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 0, 25)
	ihdr = append(ihdr, 0, 0, 0, 13, 'I', 'H', 'D', 'R')
	ihdr = binary.BigEndian.AppendUint32(ihdr, width)
	ihdr = binary.BigEndian.AppendUint32(ihdr, height)
	ihdr = append(ihdr, 8, 6, 0, 0, 0)
	ihdr = binary.BigEndian.AppendUint32(ihdr, crc32.ChecksumIEEE(ihdr[4:]))
	buf.Write(ihdr)
	return buf.Bytes()
}

// progressiveJPEGHeader is a JPEG whose SOF2 (progressive) frame header
// declares width x height with three components, followed by the start of
// a scan and nothing else — the shape of the 126-byte reproduction that
// cost 700 MB under the old pixel limit, since image/jpeg allocates every
// coefficient block on the scan header alone. The SOS marker is there for
// DecodeConfig's sake: without a JFIF segment it reads on past the frame
// header looking for an Adobe colour-space marker, and stops only at SOS
func progressiveJPEGHeader(width, height uint16) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xD8})
	sof := []byte{0xFF, 0xC2, 0x00, 0x11, 0x08}
	sof = binary.BigEndian.AppendUint16(sof, height)
	sof = binary.BigEndian.AppendUint16(sof, width)
	sof = append(sof, 0x03, 0x01, 0x22, 0x00, 0x02, 0x11, 0x01, 0x03, 0x11, 0x01)
	buf.Write(sof)
	buf.Write([]byte{0xFF, 0xDA, 0x00, 0x0C})
	return buf.Bytes()
}

// A byte cap on the input is not a bound on the decode: a header a few
// dozen bytes long can declare an image whose decoder allocates gigabytes
// before reading a single pixel. The header is read on its own first so
// an oversized image is refused before any pixel buffer exists, and the
// refusal is a property of the bytes, so it carries ErrUnsupportedCover
func TestStoreRefusesAnImageOverThePixelLimit(t *testing.T) {
	cases := map[string][]byte{
		"png 40000x40000":            pngHeader(40000, 40000),
		"progressive jpeg 7000x7000": progressiveJPEGHeader(7000, 7000),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Store(t.TempDir(), "huge", raw)
			if err == nil {
				t.Fatal("Store: want an error for an image over maxPixels")
			}
			if !strings.Contains(err.Error(), "pixel limit") {
				t.Errorf("error = %v, want it to name the pixel limit", err)
			}
			if !errors.Is(err, ErrUnsupportedCover) {
				t.Errorf("error = %v, want it to wrap ErrUnsupportedCover", err)
			}
		})
	}
}

// 4000x4000 is the ceiling the limit was sized at: the progressive JPEG
// worst case there is a few hundred megabytes, which is survivable, and a
// real cover never approaches it. A header declaring exactly that must
// pass the pixel check — the failure that follows is the missing image
// data, not the size
func TestStorePixelLimitAdmitsFourThousandSquare(t *testing.T) {
	_, err := Store(t.TempDir(), "edge", pngHeader(4000, 4000))
	if err == nil {
		t.Fatal("Store: want an error for a header with no image data")
	}
	if strings.Contains(err.Error(), "pixel limit") {
		t.Errorf("error = %v, want 4000x4000 to pass the pixel limit", err)
	}
}

// The byte cap is checked before the header is even parsed: the bytes here
// are not an image at all, and the error must still be about their size,
// so a caller reading MaxCoverBytes knows exactly where the line is
func TestStoreRefusesRawOverTheByteLimit(t *testing.T) {
	raw := bytes.Repeat([]byte{0xFF}, MaxCoverBytes+1)

	_, err := Store(t.TempDir(), "big", raw)
	if err == nil {
		t.Fatal("Store: want an error for raw over MaxCoverBytes")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error = %v, want it to name the byte limit", err)
	}
	if !errors.Is(err, ErrUnsupportedCover) {
		t.Errorf("error = %v, want it to wrap ErrUnsupportedCover", err)
	}
}

// A BMP is a real image in a format nothing here registers, which is the
// shape of a permanent failure: the same bytes fail the same way on every
// sweep, so the scanner must be told not to try again
func TestStoreRefusesAnUnregisteredFormat(t *testing.T) {
	bmp := append([]byte("BM"), make([]byte, 60)...)

	_, err := Store(t.TempDir(), "bmp", bmp)
	if err == nil {
		t.Fatal("Store: want an error for a BMP")
	}
	if !errors.Is(err, ErrUnsupportedCover) {
		t.Errorf("error = %v, want it to wrap ErrUnsupportedCover", err)
	}
}

// A 20x30 lossless WebP, produced by cwebp 1.6.0 from a PNG gradient. WebP
// is an EPUB 3.3 core media type and the format most likely to arrive as
// an embedded cover that the standard library alone cannot decode
var webpCover = []byte{
	0x52, 0x49, 0x46, 0x46, 0x2e, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50,
	0x56, 0x50, 0x38, 0x4c, 0x22, 0x00, 0x00, 0x00, 0x2f, 0x13, 0x40, 0x07,
	0x00, 0xb9, 0x32, 0x44, 0xf4, 0x3f, 0x76, 0x11, 0xd1, 0xff, 0x00, 0x91,
	0xb6, 0x4d, 0xe1, 0xfd, 0xab, 0x1e, 0x3e, 0x1a, 0x86, 0xd1, 0x8a, 0x09,
	0xa0, 0xca, 0x81, 0xf2, 0x2f, 0x00,
}

func TestStoreDecodesWebP(t *testing.T) {
	path, err := Store(t.TempDir(), "webp", webpCover)
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if w, h := decodedSize(t, path); w != 20 || h != 30 {
		t.Errorf("stored size = %dx%d, want 20x30", w, h)
	}
}

// The sentinel separates the image from the filesystem: a store that fails
// because the directory cannot be created is worth retrying on the next
// sweep, and tagging it as the image's fault would make the scanner give
// up on a perfectly good cover
func TestStoreIOFailureIsNotUnsupported(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "covers")
	if err := os.WriteFile(dir, []byte("blocks directory creation"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Store(dir, "io", solidPNG(t, 20, 30))
	if err == nil {
		t.Fatal("Store: want an error when the directory cannot be created")
	}
	if errors.Is(err, ErrUnsupportedCover) {
		t.Errorf("error = %v, want an I/O failure not to wrap ErrUnsupportedCover", err)
	}
}
