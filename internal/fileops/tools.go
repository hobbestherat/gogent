package fileops

import (
	"fmt"
)

// FileTool is the interface for file operation tools
type FileTool interface {
	Name() string
	Description() string
	Execute(args map[string]interface{}) (interface{}, error)
}

// ReadTool reads files
type ReadTool struct {
	fileSys    *FileSystem
	location   *LocationMutation
	permission *PermissionService
}

// NewReadTool creates a new read tool
func NewReadTool(fileSys *FileSystem, location *LocationMutation, permission *PermissionService) *ReadTool {
	return &ReadTool{
		fileSys:    fileSys,
		location:   location,
		permission: permission,
	}
}

func (rt *ReadTool) Name() string { return "read" }

func (rt *ReadTool) Description() string {
	return "Read a file from the workspace. Supports relative and absolute paths."
}

func (rt *ReadTool) Execute(args map[string]interface{}) (interface{}, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument")
	}

	resource, err := rt.location.GetResource(path)
	if err != nil {
		return nil, err
	}

	if err := rt.permission.Assert("read", resource); err != nil {
		if _, ok := err.(*PermissionRequiredError); ok {
			return nil, fmt.Errorf("permission required for reading %s", path)
		}
		return nil, err
	}

	content, err := rt.fileSys.Read(path)
	if err != nil {
		return nil, err
	}

	return string(content), nil
}

// WriteTool writes files
type WriteTool struct {
	fileMutation *FileMutation
	location     *LocationMutation
	permission   *PermissionService
}

// NewWriteTool creates a new write tool
func NewWriteTool(fileMutation *FileMutation, location *LocationMutation, permission *PermissionService) *WriteTool {
	return &WriteTool{
		fileMutation: fileMutation,
		location:     location,
		permission:   permission,
	}
}

func (wt *WriteTool) Name() string { return "write" }

func (wt *WriteTool) Description() string {
	return "Write content to a file. Supports relative and absolute paths. Returns operation details."
}

func (wt *WriteTool) Execute(args map[string]interface{}) (interface{}, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument: path")
	}

	content, ok := args["content"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument: content")
	}

	isExternal, err := wt.location.IsExternal(path)
	if err != nil {
		return nil, err
	}

	if isExternal {
		resource, _ := wt.location.GetResource(path)
		if err := wt.permission.Assert("external_directory", resource); err != nil {
			return nil, fmt.Errorf("external directory access requires permission: %v", err)
		}
	}

	resource, err := wt.location.GetResource(path)
	if err != nil {
		return nil, err
	}

	if err := wt.permission.Assert("edit", resource); err != nil {
		if _, ok := err.(*PermissionRequiredError); ok {
			return nil, fmt.Errorf("permission required for writing to %s", path)
		}
		return nil, err
	}

	result, err := wt.fileMutation.Write(path, []byte(content))
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"operation": result.Operation,
		"path":      result.Path,
		"resource":  result.Resource,
		"existed":   result.Existed,
		"size":      len(content),
	}, nil
}

// EditTool edits files using exact text replacement
type EditTool struct {
	fileMutation *FileMutation
	location     *LocationMutation
	permission   *PermissionService
}

// NewEditTool creates a new edit tool
func NewEditTool(fileMutation *FileMutation, location *LocationMutation, permission *PermissionService) *EditTool {
	return &EditTool{
		fileMutation: fileMutation,
		location:     location,
		permission:   permission,
	}
}

func (et *EditTool) Name() string { return "edit" }

func (et *EditTool) Description() string {
	return "Edit a file by replacing exact text. Uses conditional write for safety. Returns operation details."
}

func (et *EditTool) Execute(args map[string]interface{}) (interface{}, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument: path")
	}

	find, ok := args["find"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument: find")
	}

	replace, ok := args["replace"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument: replace")
	}

	isExternal, err := et.location.IsExternal(path)
	if err != nil {
		return nil, err
	}

	if isExternal {
		resource, _ := et.location.GetResource(path)
		if err := et.permission.Assert("external_directory", resource); err != nil {
			return nil, fmt.Errorf("external directory access requires permission: %v", err)
		}
	}

	resource, err := et.location.GetResource(path)
	if err != nil {
		return nil, err
	}

	if err := et.permission.Assert("edit", resource); err != nil {
		if _, ok := err.(*PermissionRequiredError); ok {
			return nil, fmt.Errorf("permission required for editing %s", path)
		}
		return nil, err
	}

	current, err := et.fileMutation.fileSys.Read(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	currentStr := string(current)

	newContent := replaceAll(currentStr, find, replace)
	if newContent == currentStr {
		return nil, fmt.Errorf("no changes made: find text not found in file")
	}

	result, err := et.fileMutation.WriteIfUnchanged(path, current, []byte(newContent))
	if err != nil {
		if _, ok := err.(*StaleContentError); ok {
			return nil, fmt.Errorf("file changed while editing: %s", path)
		}
		return nil, err
	}

	return map[string]interface{}{
		"operation": result.Operation,
		"path":      result.Path,
		"resource":  result.Resource,
		"existed":   result.Existed,
		"find":      find,
		"replace":   replace,
	}, nil
}
