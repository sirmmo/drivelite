package source

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// s3Client is a minimal S3 client covering the three calls drivelite needs:
// ListObjectsV2, HeadObject and GetObject.
//
// It signs requests with AWS Signature Version 4 using only the standard
// library, which keeps the project free of the (very large) AWS SDK. It works
// against AWS S3 and any compatible implementation — MinIO, Ceph, Garage,
// Backblaze B2, Cloudflare R2 and so on.
type s3Client struct {
	endpoint  *url.URL
	bucket    string
	region    string
	accessKey string
	secretKey string
	token     string // optional STS session token
	pathStyle bool
	http      *http.Client
}

// emptyPayload is sha256 of the empty string, the payload hash for GET/HEAD.
const emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func newS3Client(endpoint, bucket, region, accessKey, secretKey, token string, pathStyle bool, timeout time.Duration) (*s3Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("S3 endpoint is required")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parsing S3 endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("S3 endpoint %q has no host", endpoint)
	}
	if bucket == "" {
		return nil, fmt.Errorf("S3 bucket is required")
	}
	if region == "" {
		region = "us-east-1"
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &s3Client{
		endpoint:  u,
		bucket:    bucket,
		region:    region,
		accessKey: accessKey,
		secretKey: secretKey,
		token:     token,
		pathStyle: pathStyle,
		// No overall client timeout: object downloads stream for as long as
		// they need. Per-request deadlines come from the request context.
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost:   16,
				ResponseHeaderTimeout: timeout,
			},
		},
	}, nil
}

// uriEncode percent-encodes a string per the SigV4 rules. Unlike
// url.QueryEscape it encodes a space as %20 and leaves ~ alone; when
// keepSlash is set, "/" is preserved so object paths stay readable.
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := range len(s) {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte('/')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// urlFor builds the request URL and the canonical path used for signing.
func (c *s3Client) urlFor(key string, query url.Values) (full string, canonicalPath string, host string) {
	host = c.endpoint.Host
	path := "/"

	if c.pathStyle {
		path = "/" + c.bucket
		if key != "" {
			path += "/" + key
		}
	} else {
		host = c.bucket + "." + c.endpoint.Host
		if key != "" {
			path = "/" + key
		}
	}

	canonicalPath = uriEncode(path, true)
	full = c.endpoint.Scheme + "://" + host + canonicalPath
	if q := canonicalQuery(query); q != "" {
		full += "?" + q
	}
	return full, canonicalPath, host
}

// canonicalQuery renders query parameters sorted and encoded as SigV4 wants.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// sign attaches the SigV4 Authorization header to a request.
func (c *s3Client) sign(req *http.Request, canonicalPath string, query url.Values, payloadHash string) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if c.token != "" {
		req.Header.Set("X-Amz-Security-Token", c.token)
	}

	// Anonymous access to a public bucket needs no signature at all.
	if c.accessKey == "" || c.secretKey == "" {
		return
	}

	// Canonical headers: host plus every x-amz-* header, lowercased and sorted.
	headers := map[string]string{"host": req.URL.Host}
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		if strings.HasPrefix(lower, "x-amz-") || lower == "range" {
			headers[lower] = strings.TrimSpace(strings.Join(values, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		names = append(names, n)
	}
	sort.Strings(names)

	var canonHeaders strings.Builder
	for _, n := range names {
		canonHeaders.WriteString(n)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(headers[n])
		canonHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalPath,
		canonicalQuery(query),
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, c.region, "s3", "aws4_request"}, "/")
	hashed := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(hashed[:]),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+c.secretKey), dateStamp)
	key = hmacSHA256(key, c.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.accessKey, scope, signedHeaders, signature))
}

// do builds, signs and performs one request.
func (c *s3Client) do(ctx context.Context, method, key string, query url.Values, extra http.Header) (*http.Response, error) {
	full, canonicalPath, host := c.urlFor(key, query)

	req, err := http.NewRequestWithContext(ctx, method, full, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	for name, values := range extra {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	c.sign(req, canonicalPath, query, emptyPayload)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 %s %s: %w", method, key, err)
	}
	return res, nil
}

// s3Error renders a failed response, including the XML error body when there
// is one, because "403" alone is a miserable thing to debug.
func s3Error(method, key string, res *http.Response) error {
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))

	var parsed struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(body, &parsed)

	switch res.StatusCode {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusForbidden, http.StatusUnauthorized:
		return fmt.Errorf("s3 %s %q denied (%s): %s — check credentials, bucket and region",
			method, key, parsed.Code, parsed.Message)
	}
	if parsed.Code != "" {
		return fmt.Errorf("s3 %s %q failed: %s: %s", method, key, parsed.Code, parsed.Message)
	}
	return fmt.Errorf("s3 %s %q failed: HTTP %d", method, key, res.StatusCode)
}

// s3Object is one key returned by a listing.
type s3Object struct {
	Key          string
	Size         int64
	LastModified time.Time
	ETag         string
}

// listResult is one page of a ListObjectsV2 response.
type listResult struct {
	Objects   []s3Object
	Prefixes  []string
	NextToken string
}

// listPage performs a single ListObjectsV2 call.
//
// With delimiter "/" the response separates keys directly under the prefix
// (Contents) from the sub-prefixes beneath it (CommonPrefixes), which is what
// gives S3 its folder-like behaviour. Passing an empty delimiter walks the
// whole subtree instead.
func (c *s3Client) listPage(ctx context.Context, prefix, delimiter, token string, maxKeys int) (*listResult, error) {
	query := url.Values{}
	query.Set("list-type", "2")
	if prefix != "" {
		query.Set("prefix", prefix)
	}
	if delimiter != "" {
		query.Set("delimiter", delimiter)
	}
	if token != "" {
		query.Set("continuation-token", token)
	}
	if maxKeys > 0 {
		query.Set("max-keys", strconv.Itoa(maxKeys))
	}

	res, err := c.do(ctx, http.MethodGet, "", query, nil)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, s3Error("LIST", prefix, res)
	}
	defer res.Body.Close()

	var payload struct {
		Contents []struct {
			Key          string    `xml:"Key"`
			Size         int64     `xml:"Size"`
			LastModified time.Time `xml:"LastModified"`
			ETag         string    `xml:"ETag"`
		} `xml:"Contents"`
		CommonPrefixes []struct {
			Prefix string `xml:"Prefix"`
		} `xml:"CommonPrefixes"`
		NextContinuationToken string `xml:"NextContinuationToken"`
		IsTruncated           bool   `xml:"IsTruncated"`
	}
	if err := xml.NewDecoder(res.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("parsing S3 listing: %w", err)
	}

	out := &listResult{}
	for _, o := range payload.Contents {
		out.Objects = append(out.Objects, s3Object{
			Key:          o.Key,
			Size:         o.Size,
			LastModified: o.LastModified,
			ETag:         strings.Trim(o.ETag, `"`),
		})
	}
	for _, p := range payload.CommonPrefixes {
		out.Prefixes = append(out.Prefixes, p.Prefix)
	}
	if payload.IsTruncated {
		out.NextToken = payload.NextContinuationToken
	}
	return out, nil
}

// head fetches an object's metadata.
func (c *s3Client) head(ctx context.Context, key string) (s3Object, error) {
	res, err := c.do(ctx, http.MethodHead, key, nil, nil)
	if err != nil {
		return s3Object{}, err
	}
	if res.StatusCode != http.StatusOK {
		return s3Object{}, s3Error("HEAD", key, res)
	}
	defer res.Body.Close()

	obj := s3Object{Key: key, ETag: strings.Trim(res.Header.Get("ETag"), `"`)}
	if n, err := strconv.ParseInt(res.Header.Get("Content-Length"), 10, 64); err == nil {
		obj.Size = n
	}
	if t, err := http.ParseTime(res.Header.Get("Last-Modified")); err == nil {
		obj.LastModified = t
	}
	return obj, nil
}

// get streams an object, optionally starting at a byte offset.
func (c *s3Client) get(ctx context.Context, key string, offset int64) (io.ReadCloser, error) {
	var extra http.Header
	if offset > 0 {
		extra = http.Header{"Range": []string{fmt.Sprintf("bytes=%d-", offset)}}
	}
	res, err := c.do(ctx, http.MethodGet, key, nil, extra)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return nil, s3Error("GET", key, res)
	}
	return res.Body, nil
}
