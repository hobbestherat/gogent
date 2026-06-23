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
	return "Read a file and return its complete contents. Use it before reasoning about, quoting, or editing a " +
		"file — never assume a file's contents without reading it first, and always read a file before editing so " +
		"your find text matches what is actually there. The path may be absolute or relative to the workspace root; " +
		"relative paths are resolved against the workspace root and absolute paths are used as-is. It returns the " +
		"whole file untruncated, so for a very large file use grep first to locate the relevant lines."
}

func (rt *ReadTool) Execute(args map[string]interface{}) (interface{}, error) {
	path, ok := args["path"].(string)
	if !ok {
		return nil, fmt.Errorf("missing path argument")
	}

	auth, err := CheckFileAccess(rt.permission, rt.location, false, path, permission.RequestContext{})
	if err != nil {
		return nil, err
	}

	content, err := rt.fileSys.Read(path, auth)
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
	return "Create a new file or completely overwrite an existing one with the given content. Use it to author a " +
		"brand-new file or to replace a file wholesale; do NOT use it to change part of an existing file — prefer " +
		"edit, which replaces only the matched text and preserves the rest. The path may be absolute or relative to " +
		"the workspace root; relative paths are resolved against the workspace root and absolute paths are used " +
		"as-is (do not prefix a path with the workspace root if it is already absolute). Writing an existing path " +
		"discards its previous contents entirely, so read it first if you mean to keep any of it. Returns operation " +
		"details."
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

	auth, err := CheckFileAccess(wt.permission, wt.location, true, path, permission.RequestContext{})
	if err != nil {
		return nil, err
	}

	result, err := wt.fileMutation.Write(path, []byte(content), auth)
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
	return "Edit a file by replacing one exact run of text with another. Use it for a precise change where you can " +
		"quote the existing text verbatim; use write instead only to create or fully overwrite a file. The path may " +
		"be absolute or relative to the workspace root; relative paths are resolved against the workspace root and " +
		"absolute paths are used as-is. The find text must match exactly once — include enough surrounding context " +
		"to make it unique, or set replace_all to true to replace every occurrence — and the edit fails if it " +
		"matches zero times or (without replace_all) more than once. Uses a conditional write for safety and " +
		"returns operation details."
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

	replaceAll, _ := args["replace_all"].(bool)

	auth, err := CheckFileAccess(et.permission, et.location, true, path, permission.RequestContext{})
	if err != nil {
		return nil, err
	}

	current, err := et.fileMutation.fileSys.Read(path, auth)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	newContent, err := editContent(string(current), find, replace, replaceAll)
	if err != nil {
		return nil, err
	}

	result, err := et.fileMutation.WriteIfUnchanged(path, current, []byte(newContent), auth)
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
