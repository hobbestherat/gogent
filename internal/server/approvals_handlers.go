package server

import (
	"net/http"

	"github.com/hobbestherat/webapi"
)

// approvalsSvc handles the async approval gate: listing pending prompts and
// delivering decisions to the blocked tool goroutines.
type approvalsSvc struct{ s *Server }

// List handles GET /approvals — every pending permission/edit-review prompt.
func (svc approvalsSvc) List(r *http.Request) (interface{}, error) {
	return svc.s.approvals.list(), nil
}

// Decide handles POST /approvals/:aid/decision. It maps the wire decision to the
// right typed value (permission vs edit review) and unblocks the waiting tool
// goroutine. Unknown/already-resolved ids return 404.
func (svc approvalsSvc) Decide(r *http.Request, req approvalDecisionRequest, aid string) (interface{}, error) {
	pending := svc.s.approvals.get(aid)
	if pending == nil {
		return nil, webapi.NewHTTPError(http.StatusNotFound, "approval not found")
	}
	var d decision
	switch pending.kind {
	case "permission":
		d.perm = parsePermDecision(req.Decision)
	case "edit_review":
		d.edit = parseEditDecision(req.Decision)
	default:
		return nil, webapi.NewHTTPError(http.StatusBadRequest, "unknown approval kind")
	}
	if !svc.s.approvals.resolve(aid, d) {
		return nil, webapi.NewHTTPError(http.StatusConflict, "approval already resolved")
	}
	return map[string]string{"id": aid, "status": "resolved"}, nil
}

// authSvc handles the password login/logout/me endpoints. These are AuthNone so
// the login flow itself isn't caught behind the auth gate.
type authSvc struct{ s *Server }

// Login handles POST /auth/login. On a matching password it sets a signed
// session cookie and returns the user identity; a mismatch returns 401. When no
// password is configured, login is a no-op success (the provider's loopback path
// already authenticates local callers, and remote callers should use a token).
func (svc authSvc) Login(r *http.Request, req loginRequest) (interface{}, error) {
	pw := svc.s.provider.cfg.password
	if pw == "" {
		// No password configured: nothing to log in to. Treat as success for a
		// local caller; a remote caller has no credential to obtain.
		return map[string]any{"authenticated": true, "scope": string(scopeHuman)}, nil
	}
	if !passwordOK(req.Password, pw) {
		return nil, webapi.NewHTTPError(http.StatusUnauthorized, "invalid password")
	}
	cookie := svc.s.provider.signer.issue(1, pw, svc.s.provider.cfg.now())
	return &webapi.CookieResponse{
		Cookies: []*http.Cookie{{
			Name:     sessionCookieName,
			Value:    cookie,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			// Secure is intentionally off so the cookie also works over plain
			// HTTP on the loopback; front remote access with TLS as appropriate.
		}},
		Data: map[string]any{"authenticated": true, "scope": string(scopeHuman)},
	}, nil
}

// Logout handles POST /auth/logout — clears the session cookie.
func (svc authSvc) Logout(r *http.Request) (interface{}, error) {
	return &webapi.CookieResponse{
		Cookies: []*http.Cookie{{
			Name: sessionCookieName, Value: "", Path: "/",
			MaxAge: -1, // delete
		}},
	}, nil
}

// Me handles GET /auth/me — reports whether the caller is authenticated and
// under which scope.
func (svc authSvc) Me(r *http.Request) (interface{}, error) {
	if user, ok := webapi.GetUser(r.Context()); ok && user != nil {
		return map[string]any{
			"authenticated": true,
			"scope":         string(svc.s.provider.scopeOf(user.ID)),
			"user_id":       user.ID,
		}, nil
	}
	return nil, webapi.NewHTTPError(http.StatusUnauthorized, "not authenticated")
}

type loginRequest struct {
	Password string `json:"password"`
}
