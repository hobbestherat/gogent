package fileops

import (
	"fmt"

	"gogent/internal/permission"
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
	permission *permission.Service
}

// NewReadTool creates a new read tool
func NewReadTool(fileSys *FileSystem, location *LocationMutation, perm *permission.Service) *ReadTool {
	return &ReadTool{
		fileSys:    fileSys,
		location:   location,
		permission: perm,
	}
}

func (rt *ReadTool) Name() string { return "read" }

func (rt *ReadTool) Description() string {
	return "Read a file. The path may be absolute or relative to the workspace root; relative paths are resolved against the workspace root, absolute paths are used as-is."
}

func (rt *ReadTool) Execute(args map[string]interface{}) (interface{}, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument")
	}

	if err := CheckFileAccess(rt.permission, rt.location, false, path); err != nil {
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
	permission   *permission.Service
}

// NewWriteTool creates a new write tool
func NewWriteTool(fileMutation *FileMutation, location *LocationMutation, perm *permission.Service) *WriteTool {
	return &WriteTool{
		fileMutation: fileMutation,
		location:     location,
		permission:   perm,
	}
}

func (wt *WriteTool) Name() string { return "write" }

func (wt *WriteTool) Description() string {
	return "Write content to a file. The path may be absolute or relative to the workspace root; relative paths are resolved against the workspace root, absolute paths are used as-is. Do not prefix a path with the workspace root if it is already absolute. Returns operation details."
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

	if err := CheckFileAccess(wt.permission, wt.location, true, path); err != nil {
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
	permission   *permission.Service
}

// NewEditTool creates a new edit tool
func NewEditTool(fileMutation *FileMutation, location *LocationMutation, perm *permission.Service) *EditTool {
	return &EditTool{
		fileMutation: fileMutation,
		location:     location,
		permission:   perm,
	}
}

func (et *EditTool) Name() string { return "edit" }

func (et *EditTool) Description() string {
	return "Edit a file by replacing exact text. The path may be absolute or relative to the workspace root; relative paths are resolved against the workspace root, absolute paths are used as-is. Uses conditional write for safety. Returns operation details."
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

	if err := CheckFileAccess(et.permission, et.location, true, path); err != nil {
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
