package server

import (
	"net/http"

	"github.com/hobbestherat/webapi"
	"gogent/internal/permission"
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
// goroutine.
//
// The endpoint is idempotent (issue #560): a decision that arrives after its
// pending approval was removed (resolved by a faster client, or auto-denied on
// timeout) is reconciled rather than hard-404'd, so the user's answer is never
// silently lost. In particular a late "always"/"always_deny" permission grant is
// persisted directly here so per-host "always allow" sticks even when the original
// prompt had already timed out. status is "resolved" for an in-time decision and
// "late" for a reconciled one; only a genuinely unknown id returns 404.
func (svc approvalsSvc) Decide(r *http.Request, req approvalDecisionRequest, aid string) (interface{}, error) {
	pending := svc.s.approvals.get(aid)
	if pending == nil {
		// Late arrival: reconcile against the recall ring so a sticky grant still
		// sticks and the client sees a benign result rather than a lost decision.
		if rec, ok := svc.s.approvals.recall(aid); ok {
			if rec.kind == "permission" && rec.permission != nil {
				if d := parsePermDecision(req.Decision); d == permission.DecisionAlways || d == permission.DecisionAlwaysDeny {
					svc.s.g.GetPermissionService().Persist(
						permission.RequestContext{SessionID: rec.sessionID, Agent: rec.agentID},
						permission.Action(rec.permission.Action), rec.permission.Resource, d)
				}
			}
			return map[string]string{"id": aid, "status": "late"}, nil
		}
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
		// Raced by another delivered decision (a double-consumer). The winner's
		// answer was applied to the in-flight call; but if the winner's was a
		// one-shot allow/deny and THIS loser's is a sticky grant, persisting only on
		// the winner would silently lose the grant. So persist a sticky permission
		// decision here too (idempotent — same key, mirrors the late-arrival path),
		// then acknowledge idempotently rather than 409.
		if pending.kind == "permission" && pending.permission != nil &&
			(d.perm == permission.DecisionAlways || d.perm == permission.DecisionAlwaysDeny) {
			svc.s.g.GetPermissionService().Persist(
				permission.RequestContext{SessionID: pending.sessionID, Agent: pending.agentID},
				permission.Action(pending.permission.Action), pending.permission.Resource, d.perm)
		}
		return map[string]string{"id": aid, "status": "resolved"}, nil
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
		// Secure is intentionally off so the cookie also works over plain HTTP on
		// the loopback; front remote access with TLS as appropriate. HttpOnly +
		// SameSite=Lax are set; the missing-Secure gosec warning is by design.
		Cookies: []*http.Cookie{{ //nolint:gosec // Secure intentionally off for loopback HTTP (see comment)
			Name:     sessionCookieName,
			Value:    cookie,
			Path:     "/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		}},
		Data: map[string]any{"authenticated": true, "scope": string(scopeHuman)},
	}, nil
}

// Logout handles POST /auth/logout — clears the session cookie.
func (svc authSvc) Logout(r *http.Request) (interface{}, error) {
	return &webapi.CookieResponse{
		// Deletion cookie (empty value, MaxAge<0): Secure/HttpOnly are irrelevant
		// for clearing, and Secure stays off to match the loopback-HTTP login cookie.
		Cookies: []*http.Cookie{{ //nolint:gosec // deletion cookie; Secure off to match login (loopback HTTP)
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
