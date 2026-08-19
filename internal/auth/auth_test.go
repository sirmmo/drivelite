package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newAuth(t *testing.T) *Authenticator {
	t.Helper()
	return New(map[string]string{"alice": "s3cret"}, []byte("test-key-0123456789abcdef"),
		time.Hour, false, false)
}

func TestCheck(t *testing.T) {
	a := newAuth(t)
	cases := []struct {
		user, pass string
		want       bool
	}{
		{"alice", "s3cret", true},
		{"alice", "wrong", false},
		{"alice", "", false},
		{"bob", "s3cret", false},
		{"", "", false},
		{"ALICE", "s3cret", false}, // usernames are case sensitive
	}
	for _, c := range cases {
		if got := a.Check(c.user, c.pass); got != c.want {
			t.Errorf("Check(%q,%q) = %v, want %v", c.user, c.pass, got, c.want)
		}
	}
}

func TestSessionRoundTrip(t *testing.T) {
	a := newAuth(t)
	c := a.Issue("alice")

	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Error("session cookie should be SameSite=Lax")
	}

	user, ok := a.Verify(c.Value)
	if !ok || user != "alice" {
		t.Fatalf("Verify = (%q,%v), want (alice,true)", user, ok)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	a := newAuth(t)
	good := a.Issue("alice").Value
	payload, sig, _ := strings.Cut(good, ".")

	bad := []string{
		"",
		"garbage",
		payload,               // signature missing
		payload + ".",         // empty signature
		payload + ".deadbeef", // wrong signature
		"Zm9v." + sig,         // payload swapped, old signature
		strings.ToUpper(payload) + "." + sig,
	}
	for _, v := range bad {
		if _, ok := a.Verify(v); ok {
			t.Errorf("Verify(%q) accepted a forged cookie", v)
		}
	}

	// A cookie signed with a different key must not validate.
	other := New(map[string]string{"alice": "s3cret"}, []byte("a-completely-different-key"),
		time.Hour, false, false)
	if _, ok := a.Verify(other.Issue("alice").Value); ok {
		t.Error("cookie from a foreign key was accepted")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	a := New(map[string]string{"alice": "s3cret"}, []byte("k"), -time.Minute, false, false)
	// A negative TTL is clamped to the default, so build an expired cookie by
	// hand instead: the payload carries a timestamp already in the past.
	a.ttl = time.Hour
	expired := a.Issue("alice")
	a.ttl = -time.Hour
	stale := a.Issue("alice")

	if _, ok := a.Verify(expired.Value); !ok {
		t.Error("fresh cookie should verify")
	}
	if _, ok := a.Verify(stale.Value); ok {
		t.Error("expired cookie must be rejected")
	}
}

// Removing an account from the configuration must invalidate its live sessions.
func TestVerifyRejectsRemovedUser(t *testing.T) {
	a := newAuth(t)
	cookie := a.Issue("alice")
	delete(a.users, "alice")

	if _, ok := a.Verify(cookie.Value); ok {
		t.Error("session of a removed user is still valid")
	}
}

func TestIdentify(t *testing.T) {
	a := newAuth(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, ok := a.Identify(r); ok {
		t.Error("bare request should not authenticate")
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(a.Issue("alice"))
	if user, ok := a.Identify(r); !ok || user != "alice" {
		t.Errorf("cookie auth failed: (%q,%v)", user, ok)
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth("alice", "s3cret")
	if user, ok := a.Identify(r); !ok || user != "alice" {
		t.Errorf("basic auth failed: (%q,%v)", user, ok)
	}

	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetBasicAuth("alice", "nope")
	if _, ok := a.Identify(r); ok {
		t.Error("bad basic auth accepted")
	}
}

func TestAnonymousMode(t *testing.T) {
	a := New(map[string]string{}, []byte("k"), time.Hour, false, true)
	if !a.Anonymous() {
		t.Fatal("Anonymous() should be true")
	}
	if user, ok := a.Identify(httptest.NewRequest(http.MethodGet, "/", nil)); !ok || user != "anonymous" {
		t.Errorf("anonymous mode should admit everyone, got (%q,%v)", user, ok)
	}
}

func TestSoleUser(t *testing.T) {
	if name, ok := newAuth(t).SoleUser(); !ok || name != "alice" {
		t.Errorf("SoleUser = (%q,%v), want (alice,true)", name, ok)
	}
	multi := New(map[string]string{"a": "1", "b": "2"}, []byte("k"), time.Hour, false, false)
	if _, ok := multi.SoleUser(); ok {
		t.Error("SoleUser must be false when several accounts exist")
	}
}

func TestRateLimit(t *testing.T) {
	a := newAuth(t)

	// The bucket holds a burst of 8.
	for i := range 8 {
		if !a.Allow("10.0.0.1:1234") {
			t.Fatalf("attempt %d should be allowed within the burst", i+1)
		}
	}
	if a.Allow("10.0.0.1:1234") {
		t.Error("9th rapid attempt should be throttled")
	}

	// Throttling is per client address.
	if !a.Allow("10.0.0.2:5678") {
		t.Error("a different IP must have its own budget")
	}

	// Tokens refill over time (0.25/sec, so 8s buys 2 attempts).
	a.mu.Lock()
	a.attempts["10.0.0.1"].last = time.Now().Add(-8 * time.Second)
	a.mu.Unlock()
	if !a.Allow("10.0.0.1:1234") {
		t.Error("bucket should refill over time")
	}
}
