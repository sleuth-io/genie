// Package claudecode reads Claude Code's MCP server configuration so
// `genie mcp import` can pull existing server declarations across
// without making the user re-enter them.
//
// Claude Code stores MCP entries in two files:
//
//   - ~/.claude.json holds (a) a top-level `mcpServers` map (user
//     scope, available in every project) and (b) per-project entries
//     under `projects[<absPath>].mcpServers` (local scope, available
//     only when Claude Code is invoked from that directory).
//   - <project>/.mcp.json holds project-scope entries (committed to
//     the repo, available to anyone who clones it).
//
// The MCP entry shape in both files is the same — a subset of genie's
// ProviderConfig — so the import command can copy fields 1:1.
package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Source labels where an entry came from. Used both for display and
// for precedence on duplicate names: project-file > project-local >
// user-scope.
type Source string

const (
	SourceProjectFile  Source = "project-file"
	SourceProjectLocal Source = "project-local"
	SourceUserScope    Source = "user-scope"
)

// MCPServer is one Claude Code MCP entry. Field names match the JSON
// keys Claude Code writes; they map 1:1 onto a subset of
// config.ProviderConfig.
type MCPServer struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Type    string            `json:"type,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Entry is one MCP server visible to the user at scan time.
type Entry struct {
	Name   string
	Source Source
	Server MCPServer
}

// Scanner reads Claude Code's MCP config. Fields are exposed so tests
// can point the scanner at temp paths; production code uses
// DefaultScanner.
type Scanner struct {
	ClaudeJSONPath string // default: $HOME/.claude.json
	ProjectMCPPath string // default: <CWD>/.mcp.json
	CWD            string // absolute; key into Claude's projects map
}

// DefaultScanner builds a Scanner against the real ~/.claude.json and
// <cwd>/.mcp.json. cwd is canonicalised to an absolute path so the
// lookup into ~/.claude.json's projects map matches.
func DefaultScanner(cwd string) (*Scanner, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home dir: %w", err)
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve cwd: %w", err)
	}
	return &Scanner{
		ClaudeJSONPath: filepath.Join(home, ".claude.json"),
		ProjectMCPPath: filepath.Join(abs, ".mcp.json"),
		CWD:            abs,
	}, nil
}

type claudeJSON struct {
	MCPServers map[string]MCPServer     `json:"mcpServers,omitempty"`
	Projects   map[string]claudeProject `json:"projects,omitempty"`
}

type claudeProject struct {
	MCPServers map[string]MCPServer `json:"mcpServers,omitempty"`
}

type projectMCP struct {
	MCPServers map[string]MCPServer `json:"mcpServers,omitempty"`
}

// Scan returns one Entry per (name, source) tuple, deduplicated by
// name with precedence project-file > project-local > user-scope.
// Missing source files are treated as empty — a fresh install of
// Claude Code with no servers is valid.
func (s *Scanner) Scan() ([]Entry, error) {
	seen := map[string]Entry{}

	cj, err := s.readClaudeJSON()
	if err != nil {
		return nil, err
	}
	for name, srv := range cj.MCPServers {
		seen[name] = Entry{Name: name, Source: SourceUserScope, Server: srv}
	}
	if proj, ok := cj.Projects[s.CWD]; ok {
		for name, srv := range proj.MCPServers {
			seen[name] = Entry{Name: name, Source: SourceProjectLocal, Server: srv}
		}
	}
	pm, err := s.readProjectMCP()
	if err != nil {
		return nil, err
	}
	for name, srv := range pm.MCPServers {
		seen[name] = Entry{Name: name, Source: SourceProjectFile, Server: srv}
	}

	entries := make([]Entry, 0, len(seen))
	for _, e := range seen {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// HasGenieUserScope reports whether ~/.claude.json's top-level
// mcpServers map already contains an entry that looks like the genie
// binary (basename of Command == "genie"). The import command uses
// this to decide whether to prompt for self-registration. Project
// scopes don't count because the goal is to register genie *globally*
// (available in every project), not just here.
func (s *Scanner) HasGenieUserScope() (bool, error) {
	cj, err := s.readClaudeJSON()
	if err != nil {
		return false, err
	}
	for _, srv := range cj.MCPServers {
		if isGenieCommand(srv.Command) {
			return true, nil
		}
	}
	return false, nil
}

func isGenieCommand(cmd string) bool {
	if cmd == "" {
		return false
	}
	return filepath.Base(cmd) == "genie"
}

// Locations reports which Claude Code config sources currently contain
// an MCP entry with a given name. Used by the import command's removal
// prompt so the user can see every place a duplicate exists before
// agreeing to delete.
type Locations struct {
	UserScope    bool
	ProjectLocal bool
	ProjectFile  bool
}

// Any reports whether the entry exists in any Claude Code source.
func (l Locations) Any() bool { return l.UserScope || l.ProjectLocal || l.ProjectFile }

// LocationsOf returns where (if anywhere) Claude Code currently has an
// MCP entry with this name. Three lookups: user-scope, project-local
// (under projects[CWD] in ~/.claude.json), and project-file
// (<CWD>/.mcp.json).
func (s *Scanner) LocationsOf(name string) (Locations, error) {
	var loc Locations
	cj, err := s.readClaudeJSON()
	if err != nil {
		return loc, err
	}
	if _, ok := cj.MCPServers[name]; ok {
		loc.UserScope = true
	}
	if proj, ok := cj.Projects[s.CWD]; ok {
		if _, ok := proj.MCPServers[name]; ok {
			loc.ProjectLocal = true
		}
	}
	pm, err := s.readProjectMCP()
	if err != nil {
		return loc, err
	}
	if _, ok := pm.MCPServers[name]; ok {
		loc.ProjectFile = true
	}
	return loc, nil
}

// WriteUserScopeMCPServer merges (name -> server) into the top-level
// mcpServers map of ~/.claude.json. All other keys in the file are
// preserved verbatim (this is a read-modify-write on a JSON map of
// json.RawMessage so neighbouring entries keep any fields we don't
// model, e.g. Claude Code's `_artifact`).
func (s *Scanner) WriteUserScopeMCPServer(name string, server MCPServer) error {
	return s.editUserScope(func(servers map[string]json.RawMessage) error {
		enc, err := json.Marshal(server)
		if err != nil {
			return fmt.Errorf("encode %q: %w", name, err)
		}
		servers[name] = enc
		return nil
	})
}

// DeleteUserScopeMCPServer removes the named entry from the top-level
// mcpServers map of ~/.claude.json. No-op if name is not present.
func (s *Scanner) DeleteUserScopeMCPServer(name string) error {
	return s.editUserScope(func(servers map[string]json.RawMessage) error {
		delete(servers, name)
		return nil
	})
}

// DeleteProjectLocalMCPServer removes the named entry from
// ~/.claude.json's projects[CWD].mcpServers map. No-op if the project
// entry doesn't exist or doesn't contain the name. Other keys under
// projects[CWD] (history, allowedTools, etc.) are preserved.
func (s *Scanner) DeleteProjectLocalMCPServer(name string) error {
	return s.editProjectLocal(func(servers map[string]json.RawMessage) error {
		delete(servers, name)
		return nil
	})
}

// DeleteProjectFileMCPServer removes the named entry from
// <CWD>/.mcp.json. No-op if the file is missing or doesn't contain the
// name. Other entries in the file are preserved. Modifies a committed
// repo file — callers should make the user aware.
func (s *Scanner) DeleteProjectFileMCPServer(name string) error {
	return s.editProjectFile(func(servers map[string]json.RawMessage) error {
		delete(servers, name)
		return nil
	})
}

func (s *Scanner) editUserScope(mutate func(map[string]json.RawMessage) error) error {
	return s.editClaudeJSON(func(top map[string]json.RawMessage) error {
		servers, err := unmarshalRawMap(top["mcpServers"], "mcpServers")
		if err != nil {
			return err
		}
		if err := mutate(servers); err != nil {
			return err
		}
		return setOrDelete(top, "mcpServers", servers)
	})
}

func (s *Scanner) editProjectLocal(mutate func(map[string]json.RawMessage) error) error {
	return s.editClaudeJSON(func(top map[string]json.RawMessage) error {
		projects, err := unmarshalRawMap(top["projects"], "projects")
		if err != nil {
			return err
		}
		project, err := unmarshalRawMap(projects[s.CWD], fmt.Sprintf("projects[%q]", s.CWD))
		if err != nil {
			return err
		}
		servers, err := unmarshalRawMap(project["mcpServers"], fmt.Sprintf("projects[%q].mcpServers", s.CWD))
		if err != nil {
			return err
		}
		if err := mutate(servers); err != nil {
			return err
		}
		if err := setOrDelete(project, "mcpServers", servers); err != nil {
			return err
		}
		// Keep the projects[CWD] entry even if its mcpServers is empty;
		// Claude Code stores other state there (history, allowedTools).
		if err := setOrDelete(projects, s.CWD, project); err != nil {
			return err
		}
		return setOrDelete(top, "projects", projects)
	})
}

func (s *Scanner) editClaudeJSON(mutate func(map[string]json.RawMessage) error) error {
	raw, err := os.ReadFile(s.ClaudeJSONPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", s.ClaudeJSONPath, err)
	}
	top := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("parse %s: %w", s.ClaudeJSONPath, err)
		}
	}
	if err := mutate(top); err != nil {
		return err
	}
	return writeJSONAtomically(s.ClaudeJSONPath, top, 0o700, 0o600)
}

func (s *Scanner) editProjectFile(mutate func(map[string]json.RawMessage) error) error {
	raw, readErr := os.ReadFile(s.ProjectMCPPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", s.ProjectMCPPath, readErr)
	}
	fileExisted := readErr == nil
	top := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &top); err != nil {
			return fmt.Errorf("parse %s: %w", s.ProjectMCPPath, err)
		}
	}
	servers, err := unmarshalRawMap(top["mcpServers"], "mcpServers")
	if err != nil {
		return err
	}
	if err := mutate(servers); err != nil {
		return err
	}
	if err := setOrDelete(top, "mcpServers", servers); err != nil {
		return err
	}
	// Don't materialise an empty .mcp.json just because the caller
	// deleted from a file that never existed.
	if !fileExisted && len(top) == 0 {
		return nil
	}
	return writeJSONAtomically(s.ProjectMCPPath, top, 0o755, 0o644)
}

func unmarshalRawMap(raw json.RawMessage, label string) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", label, err)
	}
	return m, nil
}

func setOrDelete(parent map[string]json.RawMessage, key string, child map[string]json.RawMessage) error {
	if len(child) == 0 {
		delete(parent, key)
		return nil
	}
	enc, err := json.Marshal(child)
	if err != nil {
		return fmt.Errorf("encode %s: %w", key, err)
	}
	parent[key] = enc
	return nil
}

func writeJSONAtomically(path string, content any, dirMode, fileMode os.FileMode) error {
	out, err := json.MarshalIndent(content, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), fileMode); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

func (s *Scanner) readClaudeJSON() (*claudeJSON, error) {
	var cj claudeJSON
	raw, err := os.ReadFile(s.ClaudeJSONPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &cj, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.ClaudeJSONPath, err)
	}
	if err := json.Unmarshal(raw, &cj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.ClaudeJSONPath, err)
	}
	return &cj, nil
}

func (s *Scanner) readProjectMCP() (*projectMCP, error) {
	var pm projectMCP
	raw, err := os.ReadFile(s.ProjectMCPPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &pm, nil
		}
		return nil, fmt.Errorf("read %s: %w", s.ProjectMCPPath, err)
	}
	if err := json.Unmarshal(raw, &pm); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.ProjectMCPPath, err)
	}
	return &pm, nil
}
