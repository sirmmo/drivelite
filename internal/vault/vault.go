// Package vault exposes a mounted directory read-only, with strict containment
// so no request can address a path outside the configured root.
package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

var (
	// ErrForbidden means the resolved path escaped the root (traversal or symlink).
	ErrForbidden = errors.New("path outside root")
	// ErrNotFound means nothing exists at the requested path.
	ErrNotFound = errors.New("not found")
)

// Kind classifies an entry for the UI.
type Kind string

const (
	KindFolder Kind = "folder"
	KindImage  Kind = "image"
	KindVideo  Kind = "video"
	KindAudio  Kind = "audio"
	KindPDF    Kind = "pdf"
	KindText   Kind = "text"
	KindOther  Kind = "other"
)

// Entry is a single row in a directory listing.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"` // slash-separated, relative to root, no leading slash
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	ModUnix  int64  `json:"modUnix"`
	Kind     Kind   `json:"kind"`
	MimeType string `json:"mime"`
	// Previewable marks formats the Go image decoders can turn into thumbnails.
	Previewable bool `json:"previewable"`
	// Playable marks media browsers can generally play inline.
	Playable bool `json:"playable"`
}

// Vault serves one directory tree.
type Vault struct {
	root string
}

// New returns a Vault rooted at an already-resolved absolute directory.
func New(root string) *Vault { return &Vault{root: root} }

// Root returns the absolute root path.
func (v *Vault) Root() string { return v.root }

// Clean normalises a user-supplied relative path to a safe slash form.
// Any "..", leading slash, or empty segment is neutralised here.
func Clean(rel string) string {
	rel = strings.TrimSpace(rel)
	rel = path.Clean("/" + strings.TrimPrefix(rel, "/"))
	return strings.TrimPrefix(rel, "/")
}

// Resolve maps a relative request path to a real absolute path inside the root.
// It rejects anything that escapes, including via symlinks.
func (v *Vault) Resolve(rel string) (string, error) {
	clean := Clean(rel)
	abs := v.root
	if clean != "" {
		abs = filepath.Join(v.root, filepath.FromSlash(clean))
	}

	// Lexical containment first — cheap, and catches the obvious cases.
	if !v.contains(abs) {
		return "", ErrForbidden
	}

	// Then resolve symlinks and re-check, so a link inside the tree cannot
	// point outside it. Missing paths are reported as not-found, not forbidden.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", ErrNotFound
	}
	if !v.contains(real) {
		return "", ErrForbidden
	}
	return real, nil
}

func (v *Vault) contains(abs string) bool {
	if abs == v.root {
		return true
	}
	return strings.HasPrefix(abs, v.root+string(os.PathSeparator))
}

// RelOf converts an absolute path inside the root back to its slash-relative form.
func (v *Vault) RelOf(abs string) (string, error) {
	rel, err := filepath.Rel(v.root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil
	}
	if strings.HasPrefix(rel, "..") {
		return "", ErrForbidden
	}
	return filepath.ToSlash(rel), nil
}

// Stat resolves a path and returns its FileInfo.
func (v *Vault) Stat(rel string) (string, os.FileInfo, error) {
	abs, err := v.Resolve(rel)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", nil, ErrNotFound
	}
	return abs, info, nil
}

// SortMode selects the ordering of a listing.
type SortMode string

const (
	SortName SortMode = "name"
	SortSize SortMode = "size"
	SortDate SortMode = "date"
)

// List reads one directory. Folders always sort ahead of files, and hidden
// entries (dotfiles) are omitted.
func (v *Vault) List(rel string, mode SortMode, desc bool) ([]Entry, error) {
	abs, info, err := v.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", rel)
	}
	dirents, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	base := Clean(rel)
	out := make([]Entry, 0, len(dirents))
	for _, de := range dirents {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		// A symlink is only listed when its target stays inside the root.
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(filepath.Join(abs, name))
			if err != nil || !v.contains(target) {
				continue
			}
			if fi, err = os.Stat(target); err != nil {
				continue
			}
		}
		// Skip sockets, devices, pipes — nothing sensible to show or download.
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			continue
		}

		child := name
		if base != "" {
			child = base + "/" + name
		}
		out = append(out, describe(name, child, fi))
	}

	sortEntries(out, mode, desc)
	return out, nil
}

func sortEntries(entries []Entry, mode SortMode, desc bool) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir // folders first, regardless of direction
		}
		var less bool
		switch mode {
		case SortSize:
			if a.Size != b.Size {
				less = a.Size < b.Size
			} else {
				less = fold(a.Name) < fold(b.Name)
			}
		case SortDate:
			if a.ModUnix != b.ModUnix {
				less = a.ModUnix < b.ModUnix
			} else {
				less = fold(a.Name) < fold(b.Name)
			}
		default:
			less = fold(a.Name) < fold(b.Name)
		}
		if desc {
			return !less
		}
		return less
	})
}

func fold(s string) string { return strings.ToLower(s) }

func describe(name, rel string, fi os.FileInfo) Entry {
	e := Entry{
		Name:    name,
		Path:    rel,
		IsDir:   fi.IsDir(),
		ModUnix: fi.ModTime().Unix(),
	}
	if fi.IsDir() {
		e.Kind = KindFolder
		return e
	}
	e.Size = fi.Size()
	e.MimeType = MimeOf(name)
	e.Kind = kindOf(e.MimeType, name)
	e.Previewable = Decodable(name)
	e.Playable = playable(name, e.MimeType)
	return e
}

// extraTypes is the authoritative extension-to-MIME table for this app.
//
// It is deliberately self-contained: Go's built-in table is tiny (no .mp4,
// no .txt) and a minimal container image has no /etc/mime.types to fall back
// on. Getting this wrong is not cosmetic — a video served as
// application/octet-stream will not play inline in any browser.
var extraTypes = map[string]string{
	// video
	".mp4": "video/mp4", ".m4v": "video/x-m4v", ".webm": "video/webm",
	".ogv": "video/ogg", ".mov": "video/quicktime", ".avi": "video/x-msvideo",
	".mkv": "video/x-matroska", ".wmv": "video/x-ms-wmv", ".flv": "video/x-flv",
	".3gp": "video/3gpp", ".mpg": "video/mpeg", ".mpeg": "video/mpeg",
	// AVCHD camcorder streams
	".mts": "video/mp2t", ".m2ts": "video/mp2t", ".ts": "video/mp2t",
	// audio
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".ogg": "audio/ogg", ".oga": "audio/ogg", ".opus": "audio/opus",
	".wav": "audio/wav", ".flac": "audio/flac", ".aiff": "audio/aiff",
	// images
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".jpe": "image/jpeg",
	".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
	".avif": "image/avif", ".bmp": "image/bmp", ".ico": "image/x-icon",
	".tif": "image/tiff", ".tiff": "image/tiff",
	".heic": "image/heic", ".heif": "image/heif",
	".svg": "image/svg+xml", ".dng": "image/x-adobe-dng",
	".cr2": "image/x-canon-cr2", ".nef": "image/x-nikon-nef",
	".arw": "image/x-sony-arw", ".raf": "image/x-fuji-raf",
	// documents and text
	".pdf": "application/pdf", ".txt": "text/plain; charset=utf-8",
	".md": "text/markdown; charset=utf-8", ".csv": "text/csv; charset=utf-8",
	".json": "application/json", ".xml": "text/xml; charset=utf-8",
	".yml": "application/yaml", ".yaml": "application/yaml",
	".log":  "text/plain; charset=utf-8",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	// archives
	".zip": "application/zip", ".gz": "application/gzip",
	".tar": "application/x-tar", ".bz2": "application/x-bzip2",
	".xz": "application/x-xz", ".7z": "application/x-7z-compressed",
	".rar": "application/vnd.rar",
}

// MimeOf guesses a content type from the file extension.
func MimeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

func kindOf(mimeType, name string) Kind {
	switch {
	case strings.HasPrefix(mimeType, "image/"):
		return KindImage
	case strings.HasPrefix(mimeType, "video/"):
		return KindVideo
	case strings.HasPrefix(mimeType, "audio/"):
		return KindAudio
	case mimeType == "application/pdf":
		return KindPDF
	case strings.HasPrefix(mimeType, "text/"),
		mimeType == "application/json",
		mimeType == "application/yaml":
		return KindText
	}
	_ = name
	return KindOther
}

// decodable lists the image formats the standard library can decode, and so
// the only ones we can build thumbnails for. Formats like WebP, HEIC and AVIF
// still display in the browser via the raw endpoint; they just get an icon
// instead of a preview.
var decodable = map[string]bool{
	".jpg": true, ".jpeg": true, ".jpe": true,
	".png": true, ".gif": true,
}

// Decodable reports whether a thumbnail can be generated for this file.
func Decodable(name string) bool {
	return decodable[strings.ToLower(filepath.Ext(name))]
}

// playable reports whether a browser can usually play the file inline.
// AVCHD/MPEG-TS is deliberately excluded: no browser plays .MTS natively.
func playable(name, mimeType string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v", ".webm", ".ogv":
		return true
	case ".mp3", ".m4a", ".oga", ".ogg", ".wav", ".flac", ".opus":
		return true
	}
	_ = mimeType
	return false
}

// DirSize walks a subtree and returns its cumulative size and file count.
// It stops early once the limit is exceeded (limit <= 0 means no limit).
func (v *Vault) DirSize(absDir string, limit int64) (size int64, files int, exceeded bool) {
	_ = filepath.WalkDir(absDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		size += fi.Size()
		files++
		if limit > 0 && size > limit {
			exceeded = true
			return fs.SkipAll
		}
		return nil
	})
	return size, files, exceeded
}
