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
// mcpServers map of ~/.claude.json. No-op if name is not present —
// callers may invoke this after a successful import without first
// confirming the entry came from user scope.
func (s *Scanner) DeleteUserScopeMCPServer(name string) error {
	return s.editUserScope(func(servers map[string]json.RawMessage) error {
		delete(servers, name)
		return nil
	})
}

func (s *Scanner) editUserScope(mutate func(map[string]json.RawMessage) error) error {
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

	servers := map[string]json.RawMessage{}
	if existing, ok := top["mcpServers"]; ok && len(existing) > 0 {
		if err := json.Unmarshal(existing, &servers); err != nil {
			return fmt.Errorf("parse mcpServers in %s: %w", s.ClaudeJSONPath, err)
		}
	}

	if err := mutate(servers); err != nil {
		return err
	}

	if len(servers) == 0 {
		delete(top, "mcpServers")
	} else {
		enc, err := json.Marshal(servers)
		if err != nil {
			return fmt.Errorf("encode mcpServers: %w", err)
		}
		top["mcpServers"] = enc
	}

	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", s.ClaudeJSONPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(s.ClaudeJSONPath), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(s.ClaudeJSONPath), err)
	}
	tmp := s.ClaudeJSONPath + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.ClaudeJSONPath); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, s.ClaudeJSONPath, err)
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
