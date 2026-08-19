// Package auth provides minimal shared-password authentication: a signed
// session cookie plus HTTP Basic for command-line clients.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// CookieName is the session cookie key.
const CookieName = "drivelite_session"

type ctxKey struct{}

// UserKey identifies the authenticated username in a request context.
var UserKey = ctxKey{}

// Authenticator validates credentials and issues session cookies.
type Authenticator struct {
	users     map[string]string
	key       []byte
	ttl       time.Duration
	secure    bool
	anonymous bool

	mu       sync.Mutex
	attempts map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds an Authenticator. When anonymous is true every request is allowed.
func New(users map[string]string, key []byte, ttl time.Duration, secure, anonymous bool) *Authenticator {
	if ttl <= 0 {
		ttl = 168 * time.Hour
	}
	return &Authenticator{
		users:     users,
		key:       key,
		ttl:       ttl,
		secure:    secure,
		anonymous: anonymous,
		attempts:  map[string]*bucket{},
	}
}

// Anonymous reports whether authentication is disabled entirely.
func (a *Authenticator) Anonymous() bool { return a.anonymous }

// SoleUser returns the username when exactly one account is configured,
// which lets the login form accept a blank username field.
func (a *Authenticator) SoleUser() (string, bool) {
	if len(a.users) != 1 {
		return "", false
	}
	for name := range a.users {
		return name, true
	}
	return "", false
}

// Check verifies a username/password pair in constant time.
//
// It always runs a comparison, even for unknown users, so response timing does
// not reveal which usernames exist.
func (a *Authenticator) Check(user, pass string) bool {
	want, ok := a.users[user]
	if !ok {
		// Compare against a fixed dummy so the work is the same either way.
		subtle.ConstantTimeCompare([]byte(pass), []byte("\x00unknown-user-placeholder"))
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1
}

// sign returns the HMAC of a session payload.
func (a *Authenticator) sign(payload string) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Issue creates a signed cookie value of the form base64(user|expiry).signature.
func (a *Authenticator) Issue(user string) *http.Cookie {
	exp := time.Now().Add(a.ttl)
	payload := base64.RawURLEncoding.EncodeToString(
		fmt.Appendf(nil, "%s|%d", user, exp.Unix()))
	return &http.Cookie{
		Name:     CookieName,
		Value:    payload + "." + a.sign(payload),
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(a.ttl / time.Second),
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// Clear returns a cookie that removes an existing session.
func (a *Authenticator) Clear() *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// Verify validates a cookie value and returns the username it carries.
func (a *Authenticator) Verify(value string) (string, bool) {
	payload, sig, ok := strings.Cut(value, ".")
	if !ok {
		return "", false
	}
	if !hmac.Equal([]byte(sig), []byte(a.sign(payload))) {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	user, expStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	// A user removed from the config must not keep a working session.
	if _, known := a.users[user]; !known {
		return "", false
	}
	return user, true
}

// Identify resolves the caller from either a session cookie or Basic auth.
func (a *Authenticator) Identify(r *http.Request) (string, bool) {
	if a.anonymous {
		return "anonymous", true
	}
	if c, err := r.Cookie(CookieName); err == nil {
		if user, ok := a.Verify(c.Value); ok {
			return user, true
		}
	}
	if user, pass, ok := r.BasicAuth(); ok && a.Check(user, pass) {
		return user, true
	}
	return "", false
}

// Allow implements a token bucket per client IP to blunt password guessing:
// a burst of 8, refilling at roughly one attempt every 4 seconds.
func (a *Authenticator) Allow(remoteAddr string) bool {
	const burst, refillPerSec = 8.0, 0.25

	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	now := time.Now()
	b, ok := a.attempts[host]
	if !ok {
		// Opportunistically drop stale buckets so the map cannot grow forever.
		if len(a.attempts) > 4096 {
			for k, v := range a.attempts {
				if now.Sub(v.last) > 10*time.Minute {
					delete(a.attempts, k)
				}
			}
		}
		b = &bucket{tokens: burst, last: now}
		a.attempts[host] = b
	}

	b.tokens += now.Sub(b.last).Seconds() * refillPerSec
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
