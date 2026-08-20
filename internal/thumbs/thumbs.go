// Package thumbs generates and caches image thumbnails using only the
// standard library's image decoders.
package thumbs

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"io"
	"os"
	"path/filepath"
	"sync"

	// Register the decoders the standard library ships with.
	_ "image/gif"
	_ "image/png"
)

var (
	// ErrTooLarge guards against decoding absurd images into memory.
	ErrTooLarge = errors.New("image exceeds the decode limit")
)

// maxPixels caps decode work at roughly 80 megapixels.
const maxPixels = 80_000_000

// defaultMaxBytes bounds how much of a source image is read into memory.
const defaultMaxBytes = 256 << 20

// Opener yields the bytes of a source image. The caller closes the reader.
type Opener func() (io.ReadCloser, error)

// Cache renders thumbnails to a directory, one file per (source, size) pair.
type Cache struct {
	dir      string
	maxEdge  int
	maxBytes int64
	sem      chan struct{}

	mu       sync.Mutex
	inFlight map[string]*sync.WaitGroup
}

// NewCache prepares the cache directory.
func NewCache(dir string, maxEdge, workers int) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, fmt.Errorf("creating thumbnail cache %q: %w", dir, err)
	}
	// Confirm the directory is actually writable rather than failing later,
	// per-request, once a user clicks something.
	probe := filepath.Join(dir, ".writable")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("thumbnail cache %q is not writable: %w", dir, err)
	}
	os.Remove(probe)

	if workers < 1 {
		workers = 1
	}
	return &Cache{
		dir:      dir,
		maxEdge:  maxEdge,
		maxBytes: defaultMaxBytes,
		sem:      make(chan struct{}, workers),
		inFlight: map[string]*sync.WaitGroup{},
	}, nil
}

// keyFor derives a cache filename from a content identity and target size.
//
// The identity must change whenever the bytes change: local and git sources
// pass modification time and size, S3 passes the object's ETag.
func (c *Cache) keyFor(identity string, edge int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%d", identity, edge))
	return hex.EncodeToString(sum[:]) + ".jpg"
}

// Get returns the path of a cached thumbnail, generating it if necessary.
// Concurrent requests for the same thumbnail collapse into one render.
func (c *Cache) Get(identity string, edge int, open Opener) (string, error) {
	if edge <= 0 || edge > c.maxEdge {
		edge = c.maxEdge
	}
	name := c.keyFor(identity, edge)
	dst := filepath.Join(c.dir, name)

	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}

	// Collapse duplicate concurrent renders of the same thumbnail.
	c.mu.Lock()
	if wg, busy := c.inFlight[name]; busy {
		c.mu.Unlock()
		wg.Wait()
		if _, err := os.Stat(dst); err == nil {
			return dst, nil
		}
		return "", errors.New("thumbnail generation failed")
	}
	wg := &sync.WaitGroup{}
	wg.Add(1)
	c.inFlight[name] = wg
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.inFlight, name)
		c.mu.Unlock()
		wg.Done()
	}()

	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	raw, err := c.read(open)
	if err != nil {
		return "", err
	}
	if err := c.render(raw, dst, edge); err != nil {
		return "", err
	}
	return dst, nil
}

// read pulls the whole source image into memory, bounded by maxBytes.
//
// Decoding needs random access and the EXIF header has to be parsed from the
// same bytes, so buffering is simpler than seeking — and it is the only
// option for a remote object anyway.
func (c *Cache) read(open Opener) ([]byte, error) {
	rc, err := open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	limited := io.LimitReader(rc, c.maxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("reading image: %w", err)
	}
	if int64(len(raw)) > c.maxBytes {
		return nil, ErrTooLarge
	}
	return raw, nil
}

// render decodes one image, scales it down, and writes a JPEG thumbnail.
func (c *Cache) render(raw []byte, dst string, edge int) error {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("reading image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return ErrTooLarge
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("decoding image: %w", err)
	}

	// JPEGs from cameras and phones commonly rely on an EXIF orientation flag.
	if orient := readOrientation(raw); orient > 1 {
		img = applyOrientation(img, orient)
	}

	thumb := scaleDown(img, edge)

	// Write to a temporary file and rename, so a crash mid-render never leaves
	// a truncated thumbnail that would be served forever afterwards.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-thumb-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := jpeg.Encode(tmp, thumb, &jpeg.Options{Quality: 82}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// scaleDown resizes an image so its longest edge is at most edge pixels,
// averaging over each source region (a box filter) for a clean result.
func scaleDown(src image.Image, edge int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return src
	}

	dw, dh := sw, sh
	if sw > edge || sh > edge {
		if sw >= sh {
			dw = edge
			dh = max(1, int(float64(sh)*float64(edge)/float64(sw)))
		} else {
			dh = edge
			dw = max(1, int(float64(sw)*float64(edge)/float64(sh)))
		}
	}
	if dw == sw && dh == sh {
		return src
	}

	// Work from an RGBA copy: direct pixel indexing is far cheaper than the
	// per-pixel At() interface call across tens of millions of pixels.
	rgba, ok := src.(*image.RGBA)
	if !ok {
		rgba = image.NewRGBA(image.Rect(0, 0, sw, sh))
		draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	}

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		y0 := y * sh / dh
		y1 := max(y0+1, (y+1)*sh/dh)
		for x := range dw {
			x0 := x * sw / dw
			x1 := max(x0+1, (x+1)*sw/dw)

			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				row := sy * rgba.Stride
				for sx := x0; sx < x1; sx++ {
					i := row + sx*4
					r += uint32(rgba.Pix[i])
					g += uint32(rgba.Pix[i+1])
					bl += uint32(rgba.Pix[i+2])
					a += uint32(rgba.Pix[i+3])
					n++
				}
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o] = uint8(r / n)
			dst.Pix[o+1] = uint8(g / n)
			dst.Pix[o+2] = uint8(bl / n)
			dst.Pix[o+3] = uint8(a / n)
		}
	}
	return dst
}

// applyOrientation rewrites pixels according to an EXIF orientation value
// (1–8), so thumbnails are not shown rotated or mirrored.
func applyOrientation(src image.Image, orient int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	swap := orient >= 5 && orient <= 8
	ow, oh := w, h
	if swap {
		ow, oh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, ow, oh))

	for y := range h {
		for x := range w {
			c := src.At(b.Min.X+x, b.Min.Y+y)
			var nx, ny int
			switch orient {
			case 2: // mirror horizontal
				nx, ny = w-1-x, y
			case 3: // rotate 180
				nx, ny = w-1-x, h-1-y
			case 4: // mirror vertical
				nx, ny = x, h-1-y
			case 5: // mirror horizontal, rotate 270 CW
				nx, ny = y, x
			case 6: // rotate 90 CW
				nx, ny = h-1-y, x
			case 7: // mirror horizontal, rotate 90 CW
				nx, ny = h-1-y, w-1-x
			case 8: // rotate 270 CW
				nx, ny = y, w-1-x
			default:
				nx, ny = x, y
			}
			dst.Set(nx, ny, c)
		}
	}
	return dst
}

// readOrientation extracts the EXIF orientation tag (0x0112) from JPEG bytes.
// It returns 1 (the no-op default) whenever the tag cannot be found.
func readOrientation(data []byte) int {
	// EXIF lives in an APP1 segment near the start of the file.
	head := data
	if len(head) > 128*1024 {
		head = head[:128*1024]
	}
	if len(head) < 4 || head[0] != 0xFF || head[1] != 0xD8 {
		return 1 // not a JPEG
	}

	i := 2
	for i+4 <= len(head) {
		if head[i] != 0xFF {
			i++
			continue
		}
		marker := head[i+1]
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if i+4 > len(head) {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(head[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(head) {
			return 1
		}
		if marker == 0xE1 {
			seg := head[i+4 : i+2+segLen]
			if len(seg) > 6 && string(seg[:4]) == "Exif" {
				if o := parseExifOrientation(seg[6:]); o > 0 {
					return o
				}
			}
			return 1
		}
		if marker == 0xDA { // start of scan — EXIF would have appeared already
			return 1
		}
		i += 2 + segLen
	}
	return 1
}

// parseExifOrientation walks a TIFF header and IFD0 looking for tag 0x0112.
func parseExifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	if bo.Uint16(tiff[2:4]) != 42 {
		return 0
	}
	offset := int(bo.Uint32(tiff[4:8]))
	if offset < 8 || offset+2 > len(tiff) {
		return 0
	}
	count := int(bo.Uint16(tiff[offset : offset+2]))
	entry := offset + 2
	for range count {
		if entry+12 > len(tiff) {
			return 0
		}
		tag := bo.Uint16(tiff[entry : entry+2])
		if tag == 0x0112 {
			// Value is a SHORT stored inline in the entry's value field.
			v := int(bo.Uint16(tiff[entry+8 : entry+10]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 0
		}
		entry += 12
	}
	return 0
}
