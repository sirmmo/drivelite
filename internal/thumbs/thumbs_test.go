package thumbs

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// makeJPEG writes a solid-colour JPEG of the given size and returns its path.
func makeJPEG(t *testing.T, dir, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			// A gradient, so downscaling has something real to average.
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGetGeneratesAndCaches(t *testing.T) {
	src := t.TempDir()
	cacheDir := t.TempDir()

	path := makeJPEG(t, src, "photo.jpg", 800, 600)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewCache(cacheDir, 400, 2)
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.Get(path, info.ModTime().Unix(), info.Size(), 200)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("thumbnail is not a decodable image: %v", err)
	}
	if cfg.Width != 200 || cfg.Height != 150 {
		t.Errorf("thumbnail is %dx%d, want 200x150 (aspect preserved)", cfg.Width, cfg.Height)
	}

	// A second call must reuse the cached file rather than re-render.
	again, err := c.Get(path, info.ModTime().Unix(), info.Size(), 200)
	if err != nil {
		t.Fatal(err)
	}
	if again != out {
		t.Errorf("cache key not stable: %q vs %q", again, out)
	}

	// A changed mtime must produce a different cache entry.
	other, err := c.Get(path, info.ModTime().Unix()+1, info.Size(), 200)
	if err != nil {
		t.Fatal(err)
	}
	if other == out {
		t.Error("cache key ignores modification time")
	}
}

func TestGetRejectsNonImage(t *testing.T) {
	src, cacheDir := t.TempDir(), t.TempDir()
	path := filepath.Join(src, "notes.txt")
	if err := os.WriteFile(path, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := NewCache(cacheDir, 400, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(path, 0, 12, 200); err == nil {
		t.Error("expected an error for a non-image file")
	}
}

func TestNewCacheRejectsUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent := t.TempDir()
	locked := filepath.Join(parent, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCache(filepath.Join(locked, "cache"), 400, 1); err == nil {
		t.Error("expected NewCache to fail on an unwritable location")
	}
}

func TestScaleDownAspectAndBounds(t *testing.T) {
	cases := []struct{ w, h, edge, wantW, wantH int }{
		{800, 600, 200, 200, 150}, // landscape
		{600, 800, 200, 150, 200}, // portrait
		{500, 500, 100, 100, 100}, // square
		{100, 80, 400, 100, 80},   // smaller than target: untouched
	}
	for _, c := range cases {
		src := image.NewRGBA(image.Rect(0, 0, c.w, c.h))
		got := scaleDown(src, c.edge)
		b := got.Bounds()
		if b.Dx() != c.wantW || b.Dy() != c.wantH {
			t.Errorf("scaleDown(%dx%d, %d) = %dx%d, want %dx%d",
				c.w, c.h, c.edge, b.Dx(), b.Dy(), c.wantW, c.wantH)
		}
	}
}

func TestScaleDownAveragesColour(t *testing.T) {
	// A uniform image must stay that colour after downscaling; a box filter
	// that indexes pixels incorrectly would smear in black.
	src := image.NewRGBA(image.Rect(0, 0, 400, 400))
	want := color.RGBA{10, 200, 90, 255}
	for y := range 400 {
		for x := range 400 {
			src.Set(x, y, want)
		}
	}
	got := scaleDown(src, 40)
	r, g, b, _ := got.At(20, 20).RGBA()
	if uint8(r>>8) != want.R || uint8(g>>8) != want.G || uint8(b>>8) != want.B {
		t.Errorf("centre pixel = (%d,%d,%d), want (%d,%d,%d)",
			r>>8, g>>8, b>>8, want.R, want.G, want.B)
	}
}

// exifJPEG splices a minimal APP1/EXIF segment carrying an orientation tag
// into an otherwise ordinary JPEG.
func exifJPEG(t *testing.T, dir string, orientation uint16) string {
	t.Helper()

	var tiff bytes.Buffer
	tiff.WriteString("II")                            // little endian
	tiff.Write([]byte{0x2A, 0x00})                    // magic 42
	tiff.Write([]byte{0x08, 0x00, 0x00, 0x00})        // IFD0 at offset 8
	tiff.Write([]byte{0x01, 0x00})                    // one entry
	tiff.Write([]byte{0x12, 0x01})                    // tag 0x0112 Orientation
	tiff.Write([]byte{0x03, 0x00})                    // type SHORT
	tiff.Write([]byte{0x01, 0x00, 0x00, 0x00})        // count 1
	tiff.Write([]byte{byte(orientation), 0x00, 0, 0}) // inline value
	tiff.Write([]byte{0x00, 0x00, 0x00, 0x00})        // no next IFD

	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	segLen := len(payload) + 2

	var base bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 40, 20))
	if err := jpeg.Encode(&base, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := base.Bytes()

	var out bytes.Buffer
	out.Write(raw[:2]) // SOI
	out.Write([]byte{0xFF, 0xE1, byte(segLen >> 8), byte(segLen)})
	out.Write(payload)
	out.Write(raw[2:])

	path := filepath.Join(dir, "exif.jpg")
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadOrientation(t *testing.T) {
	dir := t.TempDir()

	for _, want := range []int{1, 3, 6, 8} {
		path := exifJPEG(t, dir, uint16(want))
		if got := readOrientation(path); got != want {
			t.Errorf("readOrientation = %d, want %d", got, want)
		}
	}

	// A plain JPEG with no EXIF reports the neutral value.
	plain := makeJPEG(t, dir, "plain.jpg", 20, 20)
	if got := readOrientation(plain); got != 1 {
		t.Errorf("readOrientation(no exif) = %d, want 1", got)
	}

	// Non-JPEG input must not panic or misreport.
	txt := filepath.Join(dir, "x.txt")
	if err := os.WriteFile(txt, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readOrientation(txt); got != 1 {
		t.Errorf("readOrientation(text) = %d, want 1", got)
	}
}

func TestApplyOrientationSwapsAxes(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 40, 20))

	// Orientations 5-8 involve a 90 degree turn, so width and height swap.
	for _, o := range []int{5, 6, 7, 8} {
		got := applyOrientation(src, o).Bounds()
		if got.Dx() != 20 || got.Dy() != 40 {
			t.Errorf("orientation %d: got %dx%d, want 20x40", o, got.Dx(), got.Dy())
		}
	}
	for _, o := range []int{2, 3, 4} {
		got := applyOrientation(src, o).Bounds()
		if got.Dx() != 40 || got.Dy() != 20 {
			t.Errorf("orientation %d: got %dx%d, want 40x20", o, got.Dx(), got.Dy())
		}
	}
}
