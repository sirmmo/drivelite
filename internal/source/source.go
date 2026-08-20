// Package source abstracts the storage that drivelite exposes.
//
// Every backend — a local directory, a git working tree, or an S3-compatible
// bucket — presents the same read-only view: a tree of folders and files
// addressed by slash-separated paths relative to a root, where "" is the root
// itself. Nothing in this package ever writes to the backing store.
package source

import (
	"context"
	"errors"
	"io"
	"mime"
	"path/filepath"
	"sort"
	"strings"
)

var (
	// ErrForbidden means the requested path resolved outside the root.
	ErrForbidden = errors.New("path outside root")
	// ErrNotFound means nothing exists at the requested path.
	ErrNotFound = errors.New("not found")
	// ErrNotSupported means a backend cannot perform the operation.
	ErrNotSupported = errors.New("not supported by this backend")
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

// Entry describes one folder or file.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"` // slash-separated, relative to the root
	IsDir    bool   `json:"isDir"`
	Size     int64  `json:"size"`
	ModUnix  int64  `json:"modUnix"`
	Kind     Kind   `json:"kind"`
	MimeType string `json:"mime"`

	// Previewable marks formats a thumbnail can be generated for.
	Previewable bool `json:"previewable"`
	// Playable marks media browsers can generally play inline.
	Playable bool `json:"playable"`

	// CacheTag identifies this exact revision of the content. It feeds the
	// thumbnail cache key, so a changed file naturally gets a new thumbnail.
	// Local and git backends use modification time and size; S3 uses the ETag.
	CacheTag string `json:"-"`
}

// File is an open, seekable handle to a file's contents. Seeking is what
// makes HTTP Range requests — and therefore video scrubbing — work.
type File interface {
	io.ReadSeeker
	io.Closer
	// Size is the total length of the content in bytes.
	Size() int64
}

// Source is a read-only tree of folders and files.
//
// Implementations must treat every path as untrusted input and refuse
// anything that escapes the root.
type Source interface {
	// Name is a short description used in logs and the UI footer.
	Name() string

	// List returns the immediate children of a directory, unsorted.
	List(ctx context.Context, dir string) ([]Entry, error)

	// Stat describes a single path. The root ("") is always a directory.
	Stat(ctx context.Context, name string) (Entry, error)

	// Open returns the contents of a file.
	Open(ctx context.Context, name string) (File, error)

	// Walk visits every file beneath dir, recursively. Directories are not
	// reported. Returning an error from fn aborts the walk.
	Walk(ctx context.Context, dir string, fn func(Entry) error) error

	// Close releases background workers and connections.
	Close() error
}

// Clean normalises a user-supplied path to a safe relative slash form.
// Any "..", leading slash or empty segment is neutralised here, so a cleaned
// path can never climb above the root.
func Clean(rel string) string {
	rel = strings.TrimSpace(rel)
	rel = strings.ReplaceAll(rel, "\\", "/")

	segments := make([]string, 0, 8)
	for _, seg := range strings.Split(rel, "/") {
		switch seg {
		case "", ".":
			// collapse empty and current-directory segments
		case "..":
			if len(segments) > 0 {
				segments = segments[:len(segments)-1]
			}
		default:
			segments = append(segments, seg)
		}
	}
	return strings.Join(segments, "/")
}

// Parent returns the containing directory of a cleaned path.
func Parent(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// Base returns the final segment of a cleaned path.
func Base(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// Join appends a child name to a directory path.
func Join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// SortMode selects the ordering of a listing.
type SortMode string

const (
	SortName SortMode = "name"
	SortSize SortMode = "size"
	SortDate SortMode = "date"
)

// Sort orders entries in place. Folders always lead, whichever direction is
// chosen, because a listing that buries the folders is hard to navigate.
func Sort(entries []Entry, mode SortMode, desc bool) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		var less bool
		switch mode {
		case SortSize:
			if a.Size != b.Size {
				less = a.Size < b.Size
			} else {
				less = strings.ToLower(a.Name) < strings.ToLower(b.Name)
			}
		case SortDate:
			if a.ModUnix != b.ModUnix {
				less = a.ModUnix < b.ModUnix
			} else {
				less = strings.ToLower(a.Name) < strings.ToLower(b.Name)
			}
		default:
			less = strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		if desc {
			return !less
		}
		return less
	})
}

// Describe fills in the derived fields of an entry from its name.
func Describe(e Entry) Entry {
	if e.IsDir {
		e.Kind = KindFolder
		e.Size = 0
		return e
	}
	e.MimeType = MimeOf(e.Name)
	e.Kind = KindOf(e.MimeType)
	e.Previewable = Decodable(e.Name)
	e.Playable = Playable(e.Name)
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
	// Source files, common once a git repository is being served.
	//
	// ".ts" is deliberately absent: it is claimed above as an MPEG transport
	// stream, and that is what drivelite has always treated it as. The
	// extension is genuinely ambiguous — TypeScript uses it too — and there is
	// no way to tell from the name alone, so the older meaning wins rather
	// than silently changing how existing media libraries are served. A
	// TypeScript file still lists and downloads correctly; it just shows a
	// video icon.
	".go": "text/plain; charset=utf-8", ".py": "text/plain; charset=utf-8",
	".js": "text/plain; charset=utf-8", ".tsx": "text/plain; charset=utf-8",
	".jsx": "text/plain; charset=utf-8", ".rs": "text/plain; charset=utf-8",
	".c": "text/plain; charset=utf-8", ".h": "text/plain; charset=utf-8",
	".cpp": "text/plain; charset=utf-8", ".java": "text/plain; charset=utf-8",
	".rb": "text/plain; charset=utf-8", ".php": "text/plain; charset=utf-8",
	".sql": "text/plain; charset=utf-8", ".sh": "text/plain; charset=utf-8",
	".toml": "text/plain; charset=utf-8", ".ini": "text/plain; charset=utf-8",
	".cfg": "text/plain; charset=utf-8", ".conf": "text/plain; charset=utf-8",
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

// KindOf maps a MIME type to a UI category.
func KindOf(mimeType string) Kind {
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

// Playable reports whether a browser can usually play the file inline.
// AVCHD/MPEG-TS is deliberately excluded: no browser plays .MTS natively.
func Playable(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v", ".webm", ".ogv":
		return true
	case ".mp3", ".m4a", ".oga", ".ogg", ".wav", ".flac", ".opus":
		return true
	}
	return false
}

// Hidden reports whether a name should be omitted from listings.
func Hidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// HasHiddenSegment reports whether any component of a path is a dotfile.
//
// Backends must refuse these paths outright, not merely omit them from
// listings. Hiding a file from the index while still serving it on request is
// no protection at all — the names are guessable, and for the git backend
// ".git/config" holds the remote URL, which is exactly where a private-repo
// access token would end up.
func HasHiddenSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if Hidden(seg) {
			return true
		}
	}
	return false
}
