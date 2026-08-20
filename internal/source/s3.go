package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// S3Options configures an S3-compatible backend.
type S3Options struct {
	Endpoint  string // https://s3.eu-west-1.amazonaws.com, http://minio:9000, ...
	Bucket    string
	Prefix    string // optional: serve only this subtree
	Region    string // default us-east-1
	AccessKey string
	SecretKey string
	Token     string        // optional STS session token
	PathStyle bool          // bucket in the path rather than the hostname
	CacheTTL  time.Duration // how long directory listings are reused
	Timeout   time.Duration // response-header timeout per request
}

// S3 serves an S3-compatible bucket as a folder tree.
//
// S3 has no real directories: it has keys with slashes in them. Listing with
// a "/" delimiter makes the service report the common prefixes, which is what
// produces folder-like behaviour here. Zero-length "folder marker" objects
// (keys ending in "/") are recognised and hidden.
type S3 struct {
	client *s3Client
	prefix string // normalised: empty, or ending in "/"
	label  string
	ttl    time.Duration

	mu    sync.Mutex
	cache map[string]cachedListing
}

type cachedListing struct {
	entries []Entry
	expires time.Time
}

// NewS3 connects to a bucket and verifies it is reachable.
func NewS3(ctx context.Context, opts S3Options) (*S3, error) {
	client, err := newS3Client(opts.Endpoint, opts.Bucket, opts.Region,
		opts.AccessKey, opts.SecretKey, opts.Token, opts.PathStyle, opts.Timeout)
	if err != nil {
		return nil, err
	}

	prefix := strings.Trim(Clean(opts.Prefix), "/")
	if prefix != "" {
		prefix += "/"
	}

	ttl := opts.CacheTTL
	if ttl < 0 {
		ttl = 0
	}

	label := "s3 " + opts.Bucket
	if prefix != "" {
		label += "/" + strings.TrimSuffix(prefix, "/")
	}

	s := &S3{client: client, prefix: prefix, label: label, ttl: ttl,
		cache: map[string]cachedListing{}}

	// Fail fast on a misconfigured bucket rather than on the first page view.
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if _, err := client.listPage(probe, prefix, "/", "", 1); err != nil {
		return nil, fmt.Errorf("cannot read bucket %q: %w", opts.Bucket, err)
	}
	return s, nil
}

// Name implements Source.
func (s *S3) Name() string { return s.label }

// Close implements Source.
func (s *S3) Close() error { return nil }

// keyOf maps a request path to a full object key.
func (s *S3) keyOf(p string) string {
	clean := Clean(p)
	if clean == "" {
		return strings.TrimSuffix(s.prefix, "/")
	}
	return s.prefix + clean
}

// dirPrefix maps a request path to the listing prefix for its children.
func (s *S3) dirPrefix(p string) string {
	clean := Clean(p)
	if clean == "" {
		return s.prefix
	}
	return s.prefix + clean + "/"
}

// pathOf maps a full object key back to a request path.
func (s *S3) pathOf(key string) string {
	return strings.TrimPrefix(key, s.prefix)
}

// List implements Source.
func (s *S3) List(ctx context.Context, dir string) ([]Entry, error) {
	clean := Clean(dir)

	if entries, ok := s.cached(clean); ok {
		return entries, nil
	}

	prefix := s.dirPrefix(dir)
	var entries []Entry
	token := ""
	found := false

	for {
		page, err := s.client.listPage(ctx, prefix, "/", token, 1000)
		if err != nil {
			return nil, err
		}
		found = found || len(page.Objects) > 0 || len(page.Prefixes) > 0

		for _, obj := range page.Objects {
			// The folder marker for this directory itself is not an entry.
			if obj.Key == prefix || strings.HasSuffix(obj.Key, "/") {
				continue
			}
			name := Base(obj.Key)
			if Hidden(name) {
				continue
			}
			entries = append(entries, Describe(Entry{
				Name:     name,
				Path:     s.pathOf(obj.Key),
				Size:     obj.Size,
				ModUnix:  obj.LastModified.Unix(),
				CacheTag: obj.ETag,
			}))
		}

		for _, p := range page.Prefixes {
			name := Base(strings.TrimSuffix(p, "/"))
			if name == "" || Hidden(name) {
				continue
			}
			entries = append(entries, Describe(Entry{
				Name:  name,
				Path:  s.pathOf(strings.TrimSuffix(p, "/")),
				IsDir: true,
			}))
		}

		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}

	// An empty result for a non-root path means the folder does not exist.
	if !found && clean != "" {
		return nil, ErrNotFound
	}

	s.store(clean, entries)
	return entries, nil
}

func (s *S3) cached(dir string) ([]Entry, bool) {
	if s.ttl <= 0 {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[dir]
	if !ok || time.Now().After(c.expires) {
		return nil, false
	}
	return c.entries, true
}

func (s *S3) store(dir string, entries []Entry) {
	if s.ttl <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep the cache from growing without bound on a deep tree.
	if len(s.cache) > 2048 {
		now := time.Now()
		for k, v := range s.cache {
			if now.After(v.expires) {
				delete(s.cache, k)
			}
		}
		if len(s.cache) > 2048 {
			s.cache = map[string]cachedListing{}
		}
	}
	s.cache[dir] = cachedListing{entries: entries, expires: time.Now().Add(s.ttl)}
}

// Stat implements Source.
func (s *S3) Stat(ctx context.Context, name string) (Entry, error) {
	clean := Clean(name)
	if clean == "" {
		return Entry{Path: "", IsDir: true, Kind: KindFolder}, nil
	}
	if HasHiddenSegment(clean) {
		return Entry{}, ErrNotFound
	}

	// Try the object first: the common case is a file.
	obj, err := s.client.head(ctx, s.keyOf(clean))
	if err == nil {
		return Describe(Entry{
			Name:     Base(clean),
			Path:     clean,
			Size:     obj.Size,
			ModUnix:  obj.LastModified.Unix(),
			CacheTag: obj.ETag,
		}), nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Entry{}, err
	}

	// Otherwise it may be a prefix, i.e. a folder.
	page, err := s.client.listPage(ctx, s.dirPrefix(clean), "/", "", 1)
	if err != nil {
		return Entry{}, err
	}
	if len(page.Objects) > 0 || len(page.Prefixes) > 0 {
		return Entry{Name: Base(clean), Path: clean, IsDir: true, Kind: KindFolder}, nil
	}
	return Entry{}, ErrNotFound
}

// Walk implements Source. Listing without a delimiter returns the whole
// subtree, so one paginated pass covers everything.
func (s *S3) Walk(ctx context.Context, dir string, fn func(Entry) error) error {
	prefix := s.dirPrefix(dir)
	token := ""
	for {
		page, err := s.client.listPage(ctx, prefix, "", token, 1000)
		if err != nil {
			return err
		}
		for _, obj := range page.Objects {
			if strings.HasSuffix(obj.Key, "/") {
				continue // folder marker
			}
			path := s.pathOf(obj.Key)
			if HasHiddenSegment(path) {
				continue
			}
			entry := Describe(Entry{
				Name:     Base(obj.Key),
				Path:     path,
				Size:     obj.Size,
				ModUnix:  obj.LastModified.Unix(),
				CacheTag: obj.ETag,
			})
			if err := fn(entry); err != nil {
				return err
			}
		}
		if page.NextToken == "" {
			return nil
		}
		token = page.NextToken

		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// Open implements Source.
func (s *S3) Open(ctx context.Context, name string) (File, error) {
	clean := Clean(name)
	if clean == "" {
		return nil, errors.New("cannot open the bucket root")
	}
	if HasHiddenSegment(clean) {
		return nil, ErrNotFound
	}
	key := s.keyOf(clean)
	obj, err := s.client.head(ctx, key)
	if err != nil {
		return nil, err
	}
	return &s3File{ctx: ctx, client: s.client, key: key, size: obj.Size}, nil
}

// s3File presents an object as a seekable stream.
//
// Seeking is free: it only moves an offset. The next Read issues a ranged GET
// from that offset, which is exactly what serving an HTTP Range request needs
// — including the size probe that http.ServeContent performs before sending
// anything.
type s3File struct {
	ctx    context.Context
	client *s3Client
	key    string
	size   int64

	offset int64
	body   io.ReadCloser
	bodyAt int64 // offset the open body is currently positioned at
}

func (f *s3File) Size() int64 { return f.size }

func (f *s3File) Read(p []byte) (int, error) {
	if f.offset >= f.size {
		return 0, io.EOF
	}
	// Re-open whenever the stream is not already where we want to read.
	if f.body == nil || f.bodyAt != f.offset {
		if f.body != nil {
			f.body.Close()
			f.body = nil
		}
		body, err := f.client.get(f.ctx, f.key, f.offset)
		if err != nil {
			return 0, err
		}
		f.body = body
		f.bodyAt = f.offset
	}

	n, err := f.body.Read(p)
	f.offset += int64(n)
	f.bodyAt += int64(n)
	if errors.Is(err, io.EOF) && f.offset < f.size {
		// The connection ended early; the next Read will resume with a fresh
		// ranged request rather than reporting a short file.
		f.body.Close()
		f.body = nil
		if n > 0 {
			return n, nil
		}
	}
	return n, err
}

func (f *s3File) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = f.size + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, errors.New("negative position")
	}
	f.offset = abs
	return abs, nil
}

func (f *s3File) Close() error {
	if f.body != nil {
		err := f.body.Close()
		f.body = nil
		return err
	}
	return nil
}
