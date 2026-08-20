package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeS3 is an in-memory stand-in for an S3-compatible service, implementing
// just the three operations drivelite uses. It records the last Authorization
// header so the signing path can be asserted on.
type fakeS3 struct {
	bucket  string
	objects map[string]string
	mod     time.Time

	lastAuth  string
	lastQuery string
	listCalls int
}

func newFakeS3(bucket string, objects map[string]string) *fakeS3 {
	return &fakeS3{
		bucket:  bucket,
		objects: objects,
		mod:     time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.lastAuth = r.Header.Get("Authorization")
	f.lastQuery = r.URL.RawQuery

	path := strings.TrimPrefix(r.URL.Path, "/")
	if !strings.HasPrefix(path, f.bucket) {
		http.Error(w, "<Error><Code>NoSuchBucket</Code></Error>", http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(strings.TrimPrefix(path, f.bucket), "/")

	if r.URL.Query().Get("list-type") == "2" {
		f.list(w, r)
		return
	}

	body, ok := f.objects[key]
	if !ok {
		http.Error(w, "<Error><Code>NoSuchKey</Code><Message>nope</Message></Error>", http.StatusNotFound)
		return
	}

	w.Header().Set("ETag", `"etag-`+key+`"`)
	w.Header().Set("Last-Modified", f.mod.Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Honour a "bytes=N-" range, which is all the client ever sends.
	if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
		spec := strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-")
		if start, err := strconv.Atoi(spec); err == nil && start < len(body) {
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
			w.Header().Set("Content-Length", strconv.Itoa(len(body)-start))
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, body[start:])
			return
		}
	}
	io.WriteString(w, body)
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	f.listCalls++
	q := r.URL.Query()
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")

	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var contents strings.Builder
	seenPrefix := map[string]bool{}
	var prefixes []string

	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				cp := prefix + rest[:i+1]
				if !seenPrefix[cp] {
					seenPrefix[cp] = true
					prefixes = append(prefixes, cp)
				}
				continue
			}
		}
		fmt.Fprintf(&contents,
			"<Contents><Key>%s</Key><Size>%d</Size>"+
				"<LastModified>%s</LastModified><ETag>&quot;etag-%s&quot;</ETag></Contents>",
			k, len(f.objects[k]), f.mod.Format(time.RFC3339), k)
	}

	var cp strings.Builder
	for _, p := range prefixes {
		fmt.Fprintf(&cp, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", p)
	}

	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprintf(w,
		`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>%s%s<IsTruncated>false</IsTruncated></ListBucketResult>`,
		contents.String(), cp.String())
}

func newTestS3(t *testing.T, objects map[string]string, prefix string) (*S3, *fakeS3) {
	t.Helper()
	fake := newFakeS3("photos", objects)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	s, err := NewS3(context.Background(), S3Options{
		Endpoint:  server.URL,
		Bucket:    "photos",
		Prefix:    prefix,
		Region:    "eu-west-1",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secret",
		PathStyle: true,
		CacheTTL:  0, // disable caching so each test sees fresh calls
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s, fake
}

var sampleObjects = map[string]string{
	"holiday/beach.jpg":      "beach-bytes",
	"holiday/sunset.jpg":     "sunset-bytes",
	"holiday/raw/DSC001.jpg": "raw-bytes",
	"holiday/":               "", // folder marker, must be hidden
	"holiday/.secret":        "hidden",
	"notes.txt":              "top level notes",
	"clips/movie.mp4":        "movie-bytes-0123456789",
}

func TestS3ListSeparatesFilesAndFolders(t *testing.T) {
	s, _ := newTestS3(t, sampleObjects, "")

	entries, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name] = e.IsDir
	}
	if isDir, ok := got["holiday"]; !ok || !isDir {
		t.Errorf("holiday should be listed as a folder, got %v", got)
	}
	if isDir, ok := got["clips"]; !ok || !isDir {
		t.Errorf("clips should be listed as a folder, got %v", got)
	}
	if isDir, ok := got["notes.txt"]; !ok || isDir {
		t.Errorf("notes.txt should be listed as a file, got %v", got)
	}
	if len(entries) != 3 {
		t.Errorf("got %d entries, want 3: %v", len(entries), names(entries))
	}
}

func TestS3ListHidesMarkersAndDotfiles(t *testing.T) {
	s, _ := newTestS3(t, sampleObjects, "")

	entries, err := s.List(context.Background(), "holiday")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name == "" || strings.HasPrefix(e.Name, ".") {
			t.Errorf("listing exposed %q", e.Name)
		}
	}

	got := names(entries)
	sort.Strings(got)
	want := []string{"beach.jpg", "raw", "sunset.jpg"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("listing = %v, want %v", got, want)
	}
}

func TestS3Stat(t *testing.T) {
	s, _ := newTestS3(t, sampleObjects, "")
	ctx := context.Background()

	root, err := s.Stat(ctx, "")
	if err != nil || !root.IsDir {
		t.Fatalf("root stat = %+v, %v", root, err)
	}

	file, err := s.Stat(ctx, "holiday/beach.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if file.IsDir || file.Size != int64(len("beach-bytes")) {
		t.Errorf("file stat = %+v", file)
	}
	if file.CacheTag != "etag-holiday/beach.jpg" {
		t.Errorf("CacheTag = %q, want the ETag", file.CacheTag)
	}
	if file.Kind != KindImage || !file.Previewable {
		t.Errorf("file not described as an image: %+v", file)
	}

	dir, err := s.Stat(ctx, "holiday")
	if err != nil {
		t.Fatal(err)
	}
	if !dir.IsDir {
		t.Errorf("holiday should stat as a directory, got %+v", dir)
	}

	if _, err := s.Stat(ctx, "nope/missing.jpg"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing key error = %v, want ErrNotFound", err)
	}
}

func TestS3OpenReadSeek(t *testing.T) {
	s, _ := newTestS3(t, sampleObjects, "")

	f, err := s.Open(context.Background(), "clips/movie.mp4")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	body := sampleObjects["clips/movie.mp4"]
	if f.Size() != int64(len(body)) {
		t.Fatalf("Size = %d, want %d", f.Size(), len(body))
	}

	// Seeking to the end and back is what http.ServeContent does first; it
	// must not cost a request or corrupt the stream.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	all, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(all) != body {
		t.Errorf("read %q, want %q", all, body)
	}

	// A ranged read from the middle, as a video seek would produce.
	if _, err := f.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(rest) != body[6:] {
		t.Errorf("ranged read = %q, want %q", rest, body[6:])
	}
}

func TestS3Walk(t *testing.T) {
	s, _ := newTestS3(t, sampleObjects, "")

	var seen []string
	err := s.Walk(context.Background(), "holiday", func(e Entry) error {
		if e.IsDir {
			t.Errorf("Walk reported a directory: %q", e.Path)
		}
		seen = append(seen, e.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(seen)

	want := []string{"holiday/beach.jpg", "holiday/raw/DSC001.jpg", "holiday/sunset.jpg"}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("walk = %v, want %v", seen, want)
	}
}

// A configured prefix must scope every operation, so nothing above it is
// reachable even by asking for it directly.
func TestS3PrefixScoping(t *testing.T) {
	s, _ := newTestS3(t, sampleObjects, "holiday")
	ctx := context.Background()

	entries, err := s.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	got := names(entries)
	sort.Strings(got)
	if strings.Join(got, ",") != "beach.jpg,raw,sunset.jpg" {
		t.Errorf("prefixed listing = %v", got)
	}

	if _, err := s.Stat(ctx, "notes.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a key outside the prefix should not be reachable, got %v", err)
	}

	// Traversal out of the prefix is neutralised by Clean before it is used.
	if _, err := s.Stat(ctx, "../notes.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("traversal out of the prefix returned %v, want ErrNotFound", err)
	}
}

func TestS3RequestsAreSigned(t *testing.T) {
	_, fake := newTestS3(t, sampleObjects, "")

	if !strings.HasPrefix(fake.lastAuth, "AWS4-HMAC-SHA256 ") {
		t.Fatalf("Authorization = %q, want a SigV4 header", fake.lastAuth)
	}
	for _, part := range []string{"Credential=AKIAEXAMPLE/", "/eu-west-1/s3/aws4_request", "SignedHeaders=", "Signature="} {
		if !strings.Contains(fake.lastAuth, part) {
			t.Errorf("Authorization %q is missing %q", fake.lastAuth, part)
		}
	}
}

func TestS3AnonymousSkipsSigning(t *testing.T) {
	fake := newFakeS3("photos", sampleObjects)
	server := httptest.NewServer(fake)
	defer server.Close()

	if _, err := NewS3(context.Background(), S3Options{
		Endpoint:  server.URL,
		Bucket:    "photos",
		PathStyle: true,
	}); err != nil {
		t.Fatal(err)
	}
	if fake.lastAuth != "" {
		t.Errorf("anonymous access sent an Authorization header: %q", fake.lastAuth)
	}
}

func TestS3ListingCache(t *testing.T) {
	fake := newFakeS3("photos", sampleObjects)
	server := httptest.NewServer(fake)
	defer server.Close()

	s, err := NewS3(context.Background(), S3Options{
		Endpoint: server.URL, Bucket: "photos", PathStyle: true,
		AccessKey: "k", SecretKey: "s", CacheTTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	before := fake.listCalls
	for range 5 {
		if _, err := s.List(context.Background(), "holiday"); err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.listCalls - before; got != 1 {
		t.Errorf("5 listings made %d requests, want 1 (the rest cached)", got)
	}
}

func TestS3MissingBucketFailsFast(t *testing.T) {
	server := httptest.NewServer(newFakeS3("photos", sampleObjects))
	defer server.Close()

	_, err := NewS3(context.Background(), S3Options{
		Endpoint: server.URL, Bucket: "wrong-bucket", PathStyle: true,
	})
	if err == nil {
		t.Fatal("expected NewS3 to fail on an unreadable bucket")
	}
	if !strings.Contains(err.Error(), "wrong-bucket") {
		t.Errorf("error should name the bucket: %v", err)
	}
}

func TestUriEncode(t *testing.T) {
	cases := []struct {
		in        string
		keepSlash bool
		want      string
	}{
		{"simple", true, "simple"},
		{"a/b", true, "a/b"},
		{"a/b", false, "a%2Fb"},
		{"with space", true, "with%20space"},
		{"hash#tag", true, "hash%23tag"},
		{"plus+sign", true, "plus%2Bsign"},
		{"tilde~ok", true, "tilde~ok"},
		{"unicode-é", true, "unicode-%C3%A9"},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.keepSlash); got != c.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", c.in, c.keepSlash, got, c.want)
		}
	}
}

func TestCanonicalQueryIsSorted(t *testing.T) {
	q := map[string][]string{
		"prefix":    {"b/"},
		"list-type": {"2"},
		"delimiter": {"/"},
		"max-keys":  {"1000"},
	}
	got := canonicalQuery(q)
	want := "delimiter=%2F&list-type=2&max-keys=1000&prefix=b%2F"
	if got != want {
		t.Errorf("canonicalQuery = %q, want %q", got, want)
	}
}
