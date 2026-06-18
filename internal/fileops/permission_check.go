package fileops

import (
	"path/filepath"

	"gogent/internal/permission"
)

// Authorization is the outcome of gating a file operation through the permission
// service (see CheckFileAccess). It is opaque: callers receive it from
// CheckFileAccess and hand it to the file operation, which relaxes the workspace
// boundary when the operation was explicitly approved for a path outside the
// workspace. Because the only way to obtain AllowsExternal()==true is to pass
// the permission gate, an Authorization cannot be forged to bypass approval.
type Authorization struct {
	// external reports that the target path lies outside the workspace and the
	// user (or a persisted rule) approved touching it. The field is unexported so
	// only CheckFileAccess — the permission gate — can set it.
	external bool
}

// AllowsExternal reports whether this authorization permits operating on a path
// outside the workspace.
func (a Authorization) AllowsExternal() bool { return a.external }

// CheckFileAccess gates a file operation through the permission service and
// returns an Authorization describing the outcome.
//
// Paths inside the workspace use the read/write actions (default-allowed by the
// workspace rules). Paths that escape the workspace use the external action
// keyed on their containing directory, so a single grant can cover a whole root
// folder. When such an external path is approved the returned Authorization
// carries AllowsExternal so the file operation can relax the workspace boundary
// — without it an approved external path would still be rejected as "escapes
// workspace". A nil service yields a workspace-only Authorization (callers that
// gate elsewhere still get boundary enforcement).
func CheckFileAccess(perm *permission.Service, loc *LocationMutation, write bool, path string) (Authorization, error) {
	return CheckFileAccessCtx(perm, loc, write, path, "", "")
}

// CheckFileAccessCtx is CheckFileAccess with session/agent attribution, so an
// "ask" prompt raised here can be routed to and badged on the requesting
// session. session and agent may be empty (headless callers use CheckFileAccess).
func CheckFileAccessCtx(perm *permission.Service, loc *LocationMutation, write bool, path, session, agent string) (Authorization, error) {
	if perm == nil || loc == nil {
		return Authorization{}, nil
	}

	external, err := loc.IsExternal(path)
	if err != nil {
		return Authorization{}, err
	}

	if external {
		abs, err := loc.ResolvePath(path)
		if err != nil {
			return Authorization{}, err
		}
		if err := perm.CheckRequest(permission.Request{
			Action:   permission.ActionExternal,
			Resource: filepath.Dir(abs),
			Detail:   path,
			Session:  session,
			Agent:    agent,
		}); err != nil {
			return Authorization{}, err
		}
		return Authorization{external: true}, nil
	}

	resource, err := loc.GetResource(path)
	if err != nil {
		return Authorization{}, err
	}

	action := permission.ActionRead
	if write {
		action = permission.ActionWrite
	}
	if err := perm.CheckRequest(permission.Request{
		Action:   action,
		Resource: resource,
		Session:  session,
		Agent:    agent,
	}); err != nil {
		return Authorization{}, err
	}
	return Authorization{}, nil
}
