package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hobbestherat/webapi"
)

// authScope distinguishes a fully-privileged human client from a restricted
// peer gogent. It is carried on an authenticated session and resolved by the
// provider (loopback/password ⇒ human; bearer token ⇒ the token's configured
// scope, defaulting to human). Sensitive endpoints (settings, models, shutdown)
// gate to human via (*Server).requireHuman.
type authScope string

const (
	scopeHuman authScope = "human"
	scopePeer  authScope = "peer"
)

// scopeRank gives scopes a total order: human (1) satisfies a peer (0)
// requirement but not vice-versa.
func scopeRank(s authScope) int {
	if s == scopeHuman {
		return 1
	}
	return 0
}

// authConfig holds the server's resolved auth configuration. A password
// authorizes binding to a non-loopback address; tokens carry an optional scope
// ("human" or "peer", defaulting to human).
type authConfig struct {
	password string               // shared password ("" disables password login)
	tokens   map[string]authScope // bearer token -> scope
	now      func() time.Time     // injectable clock (tests)
}

// composingProvider resolves a session by trying loopback → password cookie →
// bearer token in order. Each strategy is one check; the composing layer picks
// the first that yields an authenticated user. It is a single
// webapi.SessionProvider, so adding a credential type is one more branch here.
type composingProvider struct {
	cfg    authConfig
	signer *cookieSigner // HMAC key for password session cookies

	// scopeByUserID maps the numeric id GetSession assigns back to the scope it
	// represents, so handlers can recover a caller's scope from webapi.GetUser
	// (which only exposes the id). Rebuilt per request, so it reflects the
	// current config without a persistent store.
	mu            sync.Mutex
	scopeByUserID map[int64]authScope
}

// webapiSession adapts an authenticated user to the webapi.Session interface.
type webapiSession struct {
	userID      int64
	displayName string
	scope       authScope
}

func (s webapiSession) GetUserID() (int64, bool)       { return s.userID, true }
func (s webapiSession) GetUserState() webapi.UserState { return webapi.UserStateComplete }
func (s webapiSession) GetDisplayName() string         { return s.displayName }

// anonymousSession is returned when no credential matches: an unauthenticated
// (userID 0) session. webapi's AuthRequired gate then turns this into a 401.
type anonymousSession struct{}

func (anonymousSession) GetUserID() (int64, bool)       { return 0, false }
func (anonymousSession) GetUserState() webapi.UserState { return webapi.UserStateUnknown }

// GetSession implements webapi.SessionProvider. It records the resolved scope
// for the assigned user id so requireHuman can recover it later.
func (p *composingProvider) GetSession(r *http.Request) (webapi.Session, error) {
	// 1) Loopback or Unix socket: a same-machine caller is the local user (human
	//    scope). A request that arrived over the daemon's Unix-domain socket has no
	//    IP RemoteAddr (it is empty/"@"), so isLoopback alone would 401 the local
	//    TUI client driving the daemon over its own 0600 socket. The socket's
	//    filesystem permissions are the access gate there — exactly as the /exit
	//    kill switch already treats a Unix-socket connection as local.
	if isLoopback(r.RemoteAddr) || isUnixRequest(r) {
		return p.issued(1, "Local", scopeHuman), nil
	}

	// 2) Password: a valid signed session cookie from a prior /auth/login.
	if p.cfg.password != "" {
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			if uid, ok := p.signer.verify(c.Value, p.cfg.password, p.cfg.now()); ok {
				return p.issued(uid, "User", scopeHuman), nil
			}
		}
	}

	// 3) Bearer token: "Authorization: Bearer <token>". The token must be an
	//    exact key in the token map (buildTokenMap defaults an unset scope to
	//    human, so a single configured token need only be presented to pass).
	if tok := bearerToken(r); tok != "" {
		if scope, ok := p.cfg.tokens[tok]; ok {
			return p.issued(2, "Token", scope), nil
		}
	}

	return anonymousSession{}, nil
}

// issued records the scope for a user id and returns the session for it. An
// empty scope normalizes to human (the least-restrictive, default scope).
func (p *composingProvider) issued(userID int64, name string, scope authScope) webapi.Session {
	if scope == "" {
		scope = scopeHuman
	}
	p.mu.Lock()
	if p.scopeByUserID == nil {
		p.scopeByUserID = make(map[int64]authScope)
	}
	p.scopeByUserID[userID] = scope
	p.mu.Unlock()
	return webapiSession{userID: userID, displayName: name, scope: scope}
}

// scopeOf returns the scope a user id authenticated with, defaulting to human
// (the least-restrictive case) for unknown ids. Anonymous (0) is human too
// since such callers never reach a permission check — they are 401'd first.
func (p *composingProvider) scopeOf(userID int64) authScope {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.scopeByUserID[userID]; ok {
		return s
	}
	return scopeHuman
}

// --- loopback & bearer helpers ----------------------------------------------

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isUnixRequest reports whether the request arrived over a Unix-domain-socket
// listener, by inspecting the listener address the http server records in the
// connection context. Such a connection is inherently local — the daemon socket
// is 0600 and filesystem-permission gated — so it is treated as a loopback
// (human-scoped) caller for the /api auth gate, mirroring the /exit kill switch.
func isUnixRequest(r *http.Request) bool {
	if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		return la.Network() == "unix"
	}
	return false
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

// --- signed session cookie --------------------------------------------------

const (
	sessionCookieName = "gogent_session"
	cookieTTL         = 24 * time.Hour
)

// cookieSigner mints and verifies HMAC-SHA256-signed session cookies. A cookie
// encodes "<uid>.<exp>.<sig>" so the server is stateless; a per-process random
// key means a restart invalidates outstanding cookies (forcing re-login). The
// password is folded into the HMAC key so changing it invalidates outstanding
// cookies without rotating the key.
type cookieSigner struct {
	key []byte
}

func newCookieSigner() *cookieSigner {
	key := make([]byte, 32)
	_, _ = rand.Read(key) // best-effort; a failed read still yields a usable (if low-entropy) key
	return &cookieSigner{key: key}
}

// issue mints a signed cookie value for the given user id.
func (c *cookieSigner) issue(uid int64, password string, now time.Time) string {
	exp := now.Add(cookieTTL).Unix()
	payload := strconv.FormatInt(uid, 10) + "." + strconv.FormatInt(exp, 10)
	mac := c.mac(payload, password)
	return payload + "." + mac
}

// verify checks a cookie value's signature and expiry against the configured
// password. Returns the user id on success.
func (c *cookieSigner) verify(value, password string, now time.Time) (int64, bool) {
	parts := strings.SplitN(value, ".", 3)
	if len(parts) != 3 {
		return 0, false
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, false
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(c.mac(payload, password))) {
		return 0, false
	}
	if now.Unix() > exp {
		return 0, false
	}
	return uid, true
}

// mac computes the base64 HMAC-SHA256 of payload keyed by (random key ||
// password).
func (c *cookieSigner) mac(payload, password string) string {
	h := hmac.New(sha256.New, append(append([]byte(nil), c.key...), []byte(password)...))
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// constant-time password comparison, mirroring the existing /exit token gate.
func passwordOK(given, want string) bool {
	return subtle.ConstantTimeCompare([]byte(given), []byte(want)) == 1
}
