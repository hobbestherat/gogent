package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hobbestherat/webapi"
)

// fixedClock returns a constant now for deterministic cookie/timing tests.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// reqWithRemote builds a request whose RemoteAddr is set (httptest otherwise
// leaves it empty), so the loopback auth path can be exercised.
func reqWithRemote(method, url, remote string) *http.Request {
	r := httptest.NewRequest(method, url, nil)
	r.RemoteAddr = remote
	return r
}

func TestLoopbackIsHuman(t *testing.T) {
	p := &composingProvider{cfg: authConfig{now: fixedClock(time.Now())}, signer: newCookieSigner()}
	s, err := p.GetSession(reqWithRemote(http.MethodGet, "/api/health", "127.0.0.1:5000"))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	uid, ok := s.GetUserID()
	if !ok || uid != 1 {
		t.Fatalf("loopback should be authenticated as user 1, got uid=%d ok=%v", uid, ok)
	}
	if got := p.scopeOf(uid); got != scopeHuman {
		t.Fatalf("loopback scope = %q, want human", got)
	}
}

func TestAnonymousWhenNoCredentialRemote(t *testing.T) {
	p := &composingProvider{cfg: authConfig{now: fixedClock(time.Now())}, signer: newCookieSigner()}
	s, err := p.GetSession(reqWithRemote(http.MethodGet, "/api/health", "10.0.0.5:5000"))
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if _, ok := s.GetUserID(); ok {
		t.Fatalf("remote caller with no credential should be anonymous")
	}
}

func TestBearerTokenAuthenticates(t *testing.T) {
	p := &composingProvider{
		cfg:    authConfig{tokens: map[string]authScope{"s3cret": scopeHuman}, now: fixedClock(time.Now())},
		signer: newCookieSigner(),
	}
	r := reqWithRemote(http.MethodGet, "/api/health", "10.0.0.5:5000")
	r.Header.Set("Authorization", "Bearer s3cret")
	s, err := p.GetSession(r)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	uid, ok := s.GetUserID()
	if !ok {
		t.Fatalf("valid token should authenticate")
	}
	if got := p.scopeOf(uid); got != scopeHuman {
		t.Fatalf("token scope = %q, want human", got)
	}
}

func TestBearerTokenPeerScope(t *testing.T) {
	p := &composingProvider{
		cfg:    authConfig{tokens: map[string]authScope{"peer-tok": scopePeer}, now: fixedClock(time.Now())},
		signer: newCookieSigner(),
	}
	r := reqWithRemote(http.MethodGet, "/api/health", "10.0.0.5:5000")
	r.Header.Set("Authorization", "Bearer peer-tok")
	s, _ := p.GetSession(r)
	uid, ok := s.GetUserID()
	if !ok {
		t.Fatalf("valid token should authenticate")
	}
	got := p.scopeOf(uid)
	if got != scopePeer {
		t.Fatalf("token scope = %q, want peer", got)
	}
	// A peer does not satisfy the human-only permission.
	if scopeRank(got) >= scopeRank(scopeHuman) {
		t.Fatalf("peer scope should not satisfy human")
	}
}

func TestWrongBearerTokenIsAnonymous(t *testing.T) {
	p := &composingProvider{
		cfg:    authConfig{tokens: map[string]authScope{"s3cret": scopeHuman}, now: fixedClock(time.Now())},
		signer: newCookieSigner(),
	}
	r := reqWithRemote(http.MethodGet, "/api/health", "10.0.0.5:5000")
	r.Header.Set("Authorization", "Bearer nope")
	s, _ := p.GetSession(r)
	if _, ok := s.GetUserID(); ok {
		t.Fatalf("wrong token should be anonymous")
	}
}

func TestSingleTokenDefaultsToHuman(t *testing.T) {
	// A single configured token with no explicit scope is treated as human.
	p := &composingProvider{
		cfg:    authConfig{tokens: map[string]authScope{"tok": ""}, now: fixedClock(time.Now())},
		signer: newCookieSigner(),
	}
	r := reqWithRemote(http.MethodGet, "/api/health", "10.0.0.5:5000")
	r.Header.Set("Authorization", "Bearer tok")
	s, _ := p.GetSession(r)
	uid, ok := s.GetUserID()
	if !ok {
		t.Fatalf("single token should authenticate")
	}
	if got := p.scopeOf(uid); got != scopeHuman {
		t.Fatalf("single-token scope = %q, want human default", got)
	}
}

func TestPasswordCookieRoundTrip(t *testing.T) {
	now := time.Now()
	p := &composingProvider{cfg: authConfig{password: "hunter2", now: fixedClock(now)}, signer: newCookieSigner()}

	// Issue a cookie and then verify GetSession accepts it from a remote caller.
	cookie := p.signer.issue(7, "hunter2", now)
	r := reqWithRemote(http.MethodGet, "/api/health", "10.0.0.5:5000")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	s, err := p.GetSession(r)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	uid, ok := s.GetUserID()
	if !ok || uid != 7 {
		t.Fatalf("valid cookie should authenticate as uid 7, got %d ok=%v", uid, ok)
	}
	if got := p.scopeOf(uid); got != scopeHuman {
		t.Fatalf("password-cookie scope = %q, want human", got)
	}
}

func TestPasswordCookieRejectsTampered(t *testing.T) {
	now := time.Now()
	p := &composingProvider{cfg: authConfig{password: "hunter2", now: fixedClock(now)}, signer: newCookieSigner()}
	cookie := p.signer.issue(7, "hunter2", now)
	// Replace the signature wholesale with a bogus one of the same shape
	// ("payload.bogus"). The payload stays valid; only the MAC is wrong.
	idx := lastIndexByte(cookie, '.')
	if idx < 0 {
		t.Fatal("cookie has no signature separator")
	}
	tampered := cookie[:idx+1] + "bogus-signature"
	r := reqWithRemote(http.MethodGet, "/api/health", "10.0.0.5:5000")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tampered})
	s, _ := p.GetSession(r)
	if _, ok := s.GetUserID(); ok {
		t.Fatalf("tampered cookie should be anonymous")
	}
}

// lastIndexByte returns the index of the last occurrence of c in s, or -1.
func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func TestPasswordCookieRejectsWrongPassword(t *testing.T) {
	// A cookie issued under one password must not validate under another, since
	// the password is folded into the HMAC key.
	now := time.Now()
	signer := newCookieSigner()
	cookie := signer.issue(7, "hunter2", now)
	if _, ok := signer.verify(cookie, "wrong-password", now); ok {
		t.Fatalf("cookie issued under one password must not verify under another")
	}
}

func TestPasswordCookieExpires(t *testing.T) {
	now := time.Now()
	signer := newCookieSigner()
	cookie := signer.issue(7, "pw", now)
	// One second past the TTL should be rejected.
	if _, ok := signer.verify(cookie, "pw", now.Add(cookieTTL+time.Second)); ok {
		t.Fatalf("expired cookie should not verify")
	}
}

func TestPasswordConstantTimeCompare(t *testing.T) {
	if !passwordOK("secret", "secret") {
		t.Fatal("matching passwords should compare equal")
	}
	if passwordOK("secret", "secret!") {
		t.Fatal("non-matching passwords should not compare equal")
	}
	if passwordOK("secret", "") {
		t.Fatal("password vs empty should not compare equal")
	}
}

func TestScopePermissionChecker(t *testing.T) {
	p := &composingProvider{
		cfg:    authConfig{tokens: map[string]authScope{"tok": scopePeer}, now: fixedClock(time.Now())},
		signer: newCookieSigner(),
	}
	// Authenticate a peer and capture its user id.
	r := reqWithRemote(http.MethodGet, "/", "10.0.0.5:1")
	r.Header.Set("Authorization", "Bearer tok")
	s, _ := p.GetSession(r)
	peerID, _ := s.GetUserID()

	// Authenticate a human (loopback) for comparison.
	hs, _ := p.GetSession(reqWithRemote(http.MethodGet, "/", "127.0.0.1:1"))
	humanID, _ := hs.GetUserID()

	c := scopePermissionChecker{provider: p}
	ctx := t.Context()
	if ok, _ := c.HasPermission(ctx, peerID, permHuman); ok {
		t.Fatal("peer scope must not satisfy human permission")
	}
	if ok, _ := c.HasPermission(ctx, humanID, permHuman); !ok {
		t.Fatal("human scope must satisfy human permission")
	}
}

func TestBearerTokenParsing(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"}, // case-insensitive scheme
		{"BEARER abc", "abc"},
		{"Token abc", ""},  // wrong scheme
		{"", ""},           // none
		{"Bearer  ", ""},   // empty after scheme
	}
	for _, c := range cases {
		r := &http.Request{Header: http.Header{}}
		if c.header != "" {
			r.Header.Set("Authorization", c.header)
		}
		if got := bearerToken(r); got != c.want {
			t.Errorf("bearerToken(%q) = %q, want %q", c.header, got, c.want)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"10.0.0.5:80", false},
		{"8.8.8.8:53", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := isLoopback(c.addr); got != c.want {
			t.Errorf("isLoopback(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// TestWebapiSessionImplements confirms our session types satisfy the
// webapi.Session interface at compile time.
func TestWebapiSessionImplements(t *testing.T) {
	var _ webapi.Session = webapiSession{}
	var _ webapi.Session = anonymousSession{}
}

// TestComposingProviderImplements confirms the provider satisfies
// webapi.SessionProvider.
func TestComposingProviderImplements(t *testing.T) {
	var _ webapi.SessionProvider = (*composingProvider)(nil)
}
