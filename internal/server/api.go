package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/hobbestherat/webapi"
	"gogent/internal/agent"
	"gogent/internal/gogent"
)

// stdLogPrintf forwards to the standard library log so slogLogger avoids an
// import cycle on internal/diag. It is the one place webapi's warnings enter.
func stdLogPrintf(format string, args ...any) { log.Printf(format, args...) }

// randText returns n bytes of random hex (2n hex characters), for opaque ids.
func randText(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // best-effort; an empty/zero slice still yields a valid (if predictable) id
	return hex.EncodeToString(b)
}

// slogLogger adapts gogent's structured logger to webapi's minimal Logger
// interface (Printf). webapi only logs internal warnings (session-provider
// failures, permission-check errors, stream-copy failures), so this is a thin
// shim that forwards to slog at Warn level.
type slogLogger struct{}

func (slogLogger) Printf(format string, args ...any) {
	// Route through the standard log package to avoid an import cycle on
	// internal/diag; webapi's warnings are operational, not user-facing.
	stdLogPrintf("webapi: "+format, args...)
}

// Server holds the assembled webapi API plus the shared state the handlers need
// (the gogent backend, the event hub, the approval bridge and the auth
// provider). It is constructed once at startup by NewServer.
type Server struct {
	g         *gogent.Gogent
	hub       *hub
	approvals *approvalBridge
	provider  *composingProvider

	// busy tracks which sessions have a turn in flight, so /messages returns 409
	// rather than queuing a second concurrent turn on the same session.
	busyMu sync.Mutex
	busy   map[string]struct{}

	api *webapi.API
}

// Options configures the server's auth. Password is the shared login password
// (empty disables password login and keeps the listener loopback-only). Token
// is a bearer token (GOGENT_HTTP_TOKEN) granting the given scope (human or peer).
// PeerBaseURL, when set, identifies this server to a peer; it is informational.
type Options struct {
	Password        string
	Token           string
	TokenScope      authScope // human (default) or peer
	ApprovalTimeout time.Duration
	now             func() time.Time // injectable clock (tests)
}

// NewServer builds the server: it wires the event hub as the session observer,
// installs the approval bridge as the permission prompter + edit reviewer, and
// assembles the *webapi.API with every endpoint registered.
func NewServer(g *gogent.Gogent, opts Options) *Server {
	if opts.now == nil {
		opts.now = time.Now
	}
	h := newHub()
	provider := &composingProvider{
		cfg: authConfig{
			password: opts.Password,
			tokens:   buildTokenMap(opts.Token, opts.TokenScope),
			now:      opts.now,
		},
		signer: newCookieSigner(),
	}
	s := &Server{
		g:         g,
		hub:       h,
		provider:  provider,
		approvals: newApprovalBridge(h, opts.ApprovalTimeout, opts.now),
		busy:      make(map[string]struct{}),
	}

	// Re-wire every already-live session's observer through the hub (sessions
	// restored at startup exist before the server). New sessions created via the
	// API get the observer in s.createSession.
	for _, id := range g.SessionIDs() {
		if us := g.GetUserSession(id); us != nil {
			us.SetObserver(h.sessionObserver(id))
		}
	}
	// NOTE: the approval bridge is not installed here. Headless mode installs it
	// via InstallApprovalGates so remote clients answer prompts through the
	// API; TUI mode leaves the workbench as the prompter. The bridge is always
	// constructed so its /approvals endpoints are available either way.

	s.api = s.buildAPI()
	return s
}

// buildTokenMap returns the token map for the provider: a single configured
// token maps to its scope (defaulting to human when unset). No token ⇒ empty.
func buildTokenMap(token string, scope authScope) map[string]authScope {
	if token == "" {
		return map[string]authScope{}
	}
	if scope == "" {
		scope = scopeHuman
	}
	return map[string]authScope{token: scope}
}

// buildAPI assembles the *webapi.API with every endpoint registered under the
// /api base path. Handler methods are method values on the per-resource service
// structs, bound positionally by webapi (path params) and by body (2nd param).
func (s *Server) buildAPI() *webapi.API {
	sess := sessionsSvc{s: s}
	msg := messagesSvc{s: s}
	ev := eventsSvc{s: s}
	appr := approvalsSvc{s: s}
	auth := authSvc{s: s}
	mods := modelsSvc{s: s}
	tools := toolsSvc{s: s}
	skills := skillsSvc{s: s}
	settings := settingsSvc{s: s}
	sys := systemSvc{s: s}

	// AuthLevel: AuthRequired for everything except the auth/login flow and the
	// public health check. webapi's provider authenticates loopback, password
	// cookie and bearer-token callers; the rest get 401. Sensitive endpoints
	// additionally re-check scope via requireHuman in the handler body (a
	// second line of defense alongside any webapi Permissions we might add).
	req := webapi.AuthRequired
	pub := webapi.AuthNone

	return &webapi.API{
		BasePath: "/api",
		Endpoints: []webapi.Endpoint{
			// --- sessions ---
			{Path: "/sessions", Method: http.MethodGet, Handler: sess.List, AuthLevel: req},
			{Path: "/sessions", Method: http.MethodPost, Handler: sess.Create, AuthLevel: req},
			{Path: "/sessions/:id", Method: http.MethodGet, Handler: sess.Get, AuthLevel: req},
			{Path: "/sessions/:id", Method: http.MethodDelete, Handler: sess.Delete, AuthLevel: req},
			{Path: "/sessions/:id/transcript", Method: http.MethodGet, Handler: sess.Transcript, AuthLevel: req},
			{Path: "/sessions/:id/stats", Method: http.MethodGet, Handler: sess.Stats, AuthLevel: req},
			{Path: "/sessions/:id/stop", Method: http.MethodPost, Handler: sess.Stop, AuthLevel: req},
			{Path: "/sessions/:id/inject", Method: http.MethodPost, Handler: sess.Inject, AuthLevel: req},
			{Path: "/sessions/:id/undo", Method: http.MethodPost, Handler: sess.Undo, AuthLevel: req},
			{Path: "/sessions/:id/rewind", Method: http.MethodPost, Handler: sess.Rewind, AuthLevel: req},
			{Path: "/sessions/:id/plan-mode", Method: http.MethodGet, Handler: sess.PlanMode, AuthLevel: req},
			{Path: "/sessions/:id/plan-mode", Method: http.MethodPut, Handler: sess.SetPlanMode, AuthLevel: req},
			{Path: "/sessions/:id/plan", Method: http.MethodGet, Handler: sess.Plan, AuthLevel: req},
			{Path: "/sessions/:id/plan/approve", Method: http.MethodPost, Handler: sess.ApprovePlan, AuthLevel: req},
			{Path: "/sessions/:id/plan/reject", Method: http.MethodPost, Handler: sess.RejectPlan, AuthLevel: req},

			// --- messages ---
			{Path: "/sessions/:id/messages", Method: http.MethodPost, Handler: msg.Send, AuthLevel: req},
			{Path: "/sessions/:id/messages/stream", Method: http.MethodPost, Handler: msg.Stream, AuthLevel: req},

			// --- events ---
			{Path: "/sessions/:id/events", Method: http.MethodGet, Handler: ev.SessionEvents, AuthLevel: req},
			{Path: "/events", Method: http.MethodGet, Handler: ev.GlobalEvents, AuthLevel: req},

			// --- approvals ---
			{Path: "/approvals", Method: http.MethodGet, Handler: appr.List, AuthLevel: req},
			{Path: "/approvals/:aid/decision", Method: http.MethodPost, Handler: appr.Decide, AuthLevel: req},

			// --- auth ---
			{Path: "/auth/login", Method: http.MethodPost, Handler: auth.Login, AuthLevel: pub},
			{Path: "/auth/logout", Method: http.MethodPost, Handler: auth.Logout, AuthLevel: req},
			{Path: "/auth/me", Method: http.MethodGet, Handler: auth.Me, AuthLevel: req},

			// --- models ---
			{Path: "/models", Method: http.MethodGet, Handler: mods.List, AuthLevel: req},
			{Path: "/models/:name", Method: http.MethodPut, Handler: mods.Update, AuthLevel: req},
			{Path: "/models/:name/scan", Method: http.MethodPost, Handler: mods.Scan, AuthLevel: req},

			// --- tools ---
			{Path: "/tools", Method: http.MethodGet, Handler: tools.List, AuthLevel: req},
			{Path: "/tools/:name/enabled", Method: http.MethodPut, Handler: tools.SetEnabled, AuthLevel: req},

			// --- skills ---
			{Path: "/skills", Method: http.MethodGet, Handler: skills.List, AuthLevel: req},
			{Path: "/skills/:name/active", Method: http.MethodPut, Handler: skills.SetActive, AuthLevel: req},
			{Path: "/skills/:name", Method: http.MethodGet, Handler: skills.Get, AuthLevel: req},

			// --- settings ---
			{Path: "/settings", Method: http.MethodGet, Handler: settings.Get, AuthLevel: req},
			{Path: "/settings", Method: http.MethodPut, Handler: settings.Set, AuthLevel: req},
			{Path: "/settings/notifications", Method: http.MethodGet, Handler: settings.NotificationsGet, AuthLevel: req},
			{Path: "/settings/notifications", Method: http.MethodPut, Handler: settings.NotificationsSet, AuthLevel: req},
			{Path: "/settings/review-edits", Method: http.MethodGet, Handler: settings.ReviewEditsGet, AuthLevel: req},
			{Path: "/settings/review-edits", Method: http.MethodPut, Handler: settings.ReviewEditsSet, AuthLevel: req},

			// --- system ---
			{Path: "/health", Method: http.MethodGet, Handler: sys.Health, AuthLevel: pub},
			{Path: "/workspace", Method: http.MethodGet, Handler: sys.Workspace, AuthLevel: req},
			{Path: "/stats", Method: http.MethodGet, Handler: sys.Stats, AuthLevel: req},
		},
		SessionProvider:   s.provider,
		PermissionChecker: scopePermissionChecker{provider: s.provider},
		Logger:            slogLogger{},
	}
}

// API returns the assembled webapi.API, ready to RegisterHandlers onto a mux.
func (s *Server) API() *webapi.API { return s.api }

// Handler returns an http.Handler (ServeMux) with the API registered. It is the
// one-call mount point for cmd/main.go.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.api.RegisterHandlers(mux)
	return mux
}

// AuthMiddleware wraps an http.Handler with the same identity check the /api
// surface uses (loopback, password cookie, or bearer token), so legacy handlers
// mounted outside webapi are gated the same way when the server is bound to a
// non-loopback host. It is a no-op for loopback callers and returns 401
// otherwise when no valid credential is presented. /health is intentionally not
// exempted by this layer — callers decide which paths to wrap.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, _ := s.provider.GetSession(r)
		if _, ok := sess.GetUserID(); ok {
			next.ServeHTTP(w, r)
			return
		}
		// Unauthenticated remote caller.
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
	})
}

// --- shared handler helpers -------------------------------------------------

// createSession ensures a live backend session exists for id, freshly creating
// one (ephemeral or persisted) when it does not, and wiring the hub as its
// observer so its events reach API subscribers. It returns the UserSession.
func (s *Server) createSession(id string, persisted bool) *agent.UserSession {
	if us := s.g.GetUserSession(id); us != nil {
		return us
	}
	var us *agent.UserSession
	if persisted {
		us = s.g.NewSession(id)
	} else {
		us = s.g.NewEphemeralSession(id)
	}
	us.SetObserver(s.hub.sessionObserver(id))
	return us
}

// markBusy claims a session for a turn, returning a release func. The second
// concurrent claim for the same session returns false (caller returns 409).
func (s *Server) markBusy(sessionID string) (func(), bool) {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if _, ok := s.busy[sessionID]; ok {
		return nil, false
	}
	s.busy[sessionID] = struct{}{}
	return func() {
		s.busyMu.Lock()
		delete(s.busy, sessionID)
		s.busyMu.Unlock()
	}, true
}

// marshalJSON marshals v to a JSON string, returning "" on error. It is a thin
// wrapper shared by the SSE producer so handler files need not import
// encoding/json for one call.
func marshalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// randomID returns a short, URL-safe id prefixed with kind. It is suitable for
// session ids that need only be unique within a process's lifetime.
func randomID(kind string) string {
	return kind + "_" + randText(8)
}
