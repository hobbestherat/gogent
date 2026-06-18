package fileops

import (
	"path/filepath"

	"gogent/internal/permission"
)

// CheckFileAccess gates a file operation through the permission service.
//
// Paths inside the workspace use the read/write actions (default-allowed by the
// workspace rules). Paths that escape the workspace use the external action
// keyed on their containing directory, so a single grant can cover a whole root
// folder. A nil service allows everything (used by callers that gate elsewhere).
func CheckFileAccess(perm *permission.Service, loc *LocationMutation, write bool, path string) error {
	if perm == nil || loc == nil {
		return nil
	}

	external, err := loc.IsExternal(path)
	if err != nil {
		return err
	}

	if external {
		abs, err := loc.ResolvePath(path)
		if err != nil {
			return err
		}
		return perm.CheckWithDetail(permission.ActionExternal, filepath.Dir(abs), path)
	}

	resource, err := loc.GetResource(path)
	if err != nil {
		return err
	}

	action := permission.ActionRead
	if write {
		action = permission.ActionWrite
	}
	return perm.Check(action, resource)
}
