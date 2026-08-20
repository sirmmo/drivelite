package thumbs

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// jpegBytes renders a gradient JPEG of the given size.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// opener returns an Opener over a fixed byte slice, counting how many times
// the source was actually read.
func opener(data []byte, calls *int) Opener {
	return func() (io.ReadCloser, error) {
		if calls != nil {
			*calls++
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
}

func TestGetGeneratesAndCaches(t *testing.T) {
	c, err := NewCache(t.TempDir(), 400, 2)
	if err != nil {
		t.Fatal(err)
	}

	data := jpegBytes(t, 800, 600)
	reads := 0

	out, err := c.Get("photo|v1", 200, opener(data, &reads))
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

	// A second call with the same identity must reuse the cached file and
	// never touch the source again — the whole point on a remote backend.
	again, err := c.Get("photo|v1", 200, opener(data, &reads))
	if err != nil {
		t.Fatal(err)
	}
	if again != out {
		t.Errorf("cache key not stable: %q vs %q", again, out)
	}
	if reads != 1 {
		t.Errorf("source was read %d times, want 1", reads)
	}

	// A changed identity (new mtime, new ETag) must produce a new entry.
	other, err := c.Get("photo|v2", 200, opener(data, &reads))
	if err != nil {
		t.Fatal(err)
	}
	if other == out {
		t.Error("cache key ignores the content identity")
	}
}

func TestGetPropagatesOpenerErrors(t *testing.T) {
	c, err := NewCache(t.TempDir(), 400, 1)
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("backend unavailable")
	_, err = c.Get("x", 200, func() (io.ReadCloser, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Errorf("Get error = %v, want the opener's error", err)
	}
}

func TestGetRejectsNonImage(t *testing.T) {
	c, err := NewCache(t.TempDir(), 400, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("notes", 200, opener([]byte("not an image"), nil)); err == nil {
		t.Error("expected an error for a non-image file")
	}
}

func TestGetRejectsOversizedSource(t *testing.T) {
	c, err := NewCache(t.TempDir(), 400, 1)
	if err != nil {
		t.Fatal(err)
	}
	c.maxBytes = 16 // absurdly small, to exercise the guard

	_, err = c.Get("big", 200, opener(jpegBytes(t, 40, 40), nil))
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("Get error = %v, want ErrTooLarge", err)
	}
}

// Concurrent requests for the same thumbnail must collapse into one render.
func TestGetCollapsesConcurrentRenders(t *testing.T) {
	c, err := NewCache(t.TempDir(), 400, 4)
	if err != nil {
		t.Fatal(err)
	}
	data := jpegBytes(t, 600, 400)

	var mu sync.Mutex
	reads := 0
	open := func() (io.ReadCloser, error) {
		mu.Lock()
		reads++
		mu.Unlock()
		return io.NopCloser(bytes.NewReader(data)), nil
	}

	var wg sync.WaitGroup
	results := make([]string, 8)
	errs := make([]error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.Get("shared|v1", 200, open)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if results[i] != results[0] {
			t.Errorf("goroutine %d got a different path", i)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if reads > 2 {
		t.Errorf("source read %d times for 8 concurrent requests", reads)
	}
}

func TestNewCacheRejectsUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	locked := filepath.Join(t.TempDir(), "locked")
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

// exifJPEG builds a JPEG carrying a minimal APP1/EXIF orientation tag.
func exifJPEG(t *testing.T, orientation uint16) []byte {
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

	raw := jpegBytes(t, 40, 20)
	var out bytes.Buffer
	out.Write(raw[:2]) // SOI
	out.Write([]byte{0xFF, 0xE1, byte(segLen >> 8), byte(segLen)})
	out.Write(payload)
	out.Write(raw[2:])
	return out.Bytes()
}

func TestReadOrientation(t *testing.T) {
	for _, want := range []int{1, 3, 6, 8} {
		if got := readOrientation(exifJPEG(t, uint16(want))); got != want {
			t.Errorf("readOrientation = %d, want %d", got, want)
		}
	}

	// A plain JPEG with no EXIF reports the neutral value.
	if got := readOrientation(jpegBytes(t, 20, 20)); got != 1 {
		t.Errorf("readOrientation(no exif) = %d, want 1", got)
	}

	// Non-JPEG input must not panic or misreport.
	for _, junk := range [][]byte{nil, []byte("nope"), []byte{0xFF, 0xD8}} {
		if got := readOrientation(junk); got != 1 {
			t.Errorf("readOrientation(%q) = %d, want 1", junk, got)
		}
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

// An EXIF-rotated photo must come out upright, not merely non-crashing.
func TestRenderAppliesOrientation(t *testing.T) {
	c, err := NewCache(t.TempDir(), 400, 1)
	if err != nil {
		t.Fatal(err)
	}
	// Orientation 6 is a 90-degree turn, so a 40x20 source becomes 20x40.
	out, err := c.Get("rotated", 400, opener(exifJPEG(t, 6), nil))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 20 || cfg.Height != 40 {
		t.Errorf("rotated thumbnail is %dx%d, want 20x40", cfg.Width, cfg.Height)
	}
}

func TestCacheFilenamesAreOpaque(t *testing.T) {
	c, err := NewCache(t.TempDir(), 400, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The identity may contain slashes and spaces; the cache filename must
	// not, or it would escape the cache directory.
	name := c.keyFor("s3|bucket|holiday/../../etc/passwd", 200)
	if strings.ContainsAny(name, "/\\ ") {
		t.Errorf("cache filename %q is not opaque", name)
	}
}
