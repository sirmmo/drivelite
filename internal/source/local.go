package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Local serves a directory on the filesystem, read-only.
type Local struct {
	root  string
	label string
}

// NewLocal returns a Local rooted at dir. The path is made absolute and has
// its symlinks resolved once, so every later containment check compares real
// paths.
func NewLocal(dir, label string) (*Local, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving %q: %w", dir, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("root %q is not readable: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("root %q is not a directory", abs)
	}
	if label == "" {
		label = "folder " + abs
	}
	return &Local{root: abs, label: label}, nil
}

// Root returns the absolute root directory.
func (l *Local) Root() string { return l.root }

// Name implements Source.
func (l *Local) Name() string { return l.label }

// Close implements Source. Nothing to release.
func (l *Local) Close() error { return nil }

// contains reports whether an absolute path is the root or sits inside it.
func (l *Local) contains(abs string) bool {
	if abs == l.root {
		return true
	}
	return strings.HasPrefix(abs, l.root+string(os.PathSeparator))
}

// resolve maps a relative request path to a real absolute path inside the
// root, rejecting anything that escapes — including via a symlink.
func (l *Local) resolve(rel string) (string, error) {
	clean := Clean(rel)

	// Dotfiles are omitted from listings, so they must not be reachable by
	// name either. Reported as not-found rather than forbidden, so probing
	// cannot confirm what exists.
	if HasHiddenSegment(clean) {
		return "", ErrNotFound
	}

	abs := l.root
	if clean != "" {
		abs = filepath.Join(l.root, filepath.FromSlash(clean))
	}

	// Lexical containment first: cheap, and catches the obvious cases.
	if !l.contains(abs) {
		return "", ErrForbidden
	}

	// Then resolve symlinks and check again, so a link inside the tree cannot
	// point outside it.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", ErrNotFound
	}
	if !l.contains(real) {
		return "", ErrForbidden
	}
	return real, nil
}

// relOf converts an absolute path inside the root back to slash-relative form.
func (l *Local) relOf(abs string) (string, error) {
	rel, err := filepath.Rel(l.root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return "", nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", ErrForbidden
	}
	return filepath.ToSlash(rel), nil
}

// entryOf builds an Entry from a FileInfo.
func entryOf(name, path string, fi os.FileInfo) Entry {
	e := Entry{
		Name:    name,
		Path:    path,
		IsDir:   fi.IsDir(),
		Size:    fi.Size(),
		ModUnix: fi.ModTime().Unix(),
	}
	if !e.IsDir {
		e.CacheTag = strconv.FormatInt(e.ModUnix, 10) + "-" + strconv.FormatInt(e.Size, 10)
	}
	return Describe(e)
}

// Stat implements Source.
func (l *Local) Stat(_ context.Context, name string) (Entry, error) {
	abs, err := l.resolve(name)
	if err != nil {
		return Entry{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Entry{}, ErrNotFound
	}
	clean := Clean(name)
	return entryOf(Base(clean), clean, fi), nil
}

// List implements Source.
func (l *Local) List(_ context.Context, dir string) ([]Entry, error) {
	abs, err := l.resolve(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, ErrNotFound
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", dir)
	}

	dirents, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}

	base := Clean(dir)
	out := make([]Entry, 0, len(dirents))
	for _, de := range dirents {
		name := de.Name()
		if Hidden(name) {
			continue
		}
		fi, err := de.Info()
		if err != nil {
			continue
		}
		// A symlink is listed only when its target stays inside the root.
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(filepath.Join(abs, name))
			if err != nil || !l.contains(target) {
				continue
			}
			if fi, err = os.Stat(target); err != nil {
				continue
			}
		}
		// Skip sockets, devices and pipes: nothing sensible to show.
		if !fi.IsDir() && !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, entryOf(name, Join(base, name), fi))
	}
	return out, nil
}

// Open implements Source.
func (l *Local) Open(_ context.Context, name string) (File, error) {
	abs, err := l.resolve(name)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, ErrNotFound
	}
	if fi.IsDir() {
		return nil, errors.New("cannot open a directory")
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, ErrNotFound
	}
	return &localFile{File: f, size: fi.Size()}, nil
}

// Walk implements Source.
func (l *Local) Walk(ctx context.Context, dir string, fn func(Entry) error) error {
	abs, err := l.resolve(dir)
	if err != nil {
		return err
	}
	return filepath.WalkDir(abs, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // unreadable subtree: skip rather than abort
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if Hidden(d.Name()) && p != abs {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := l.relOf(p)
		if err != nil {
			return nil
		}
		return fn(entryOf(d.Name(), rel, fi))
	})
}

// localFile adapts *os.File to the File interface.
type localFile struct {
	*os.File
	size int64
}

func (f *localFile) Size() int64 { return f.size }
