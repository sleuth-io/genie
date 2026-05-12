package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestScan_PrecedenceAndDedup(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	claudeJSONPath := filepath.Join(dir, ".claude.json")
	writeJSON(t, claudeJSONPath, map[string]any{
		"mcpServers": map[string]any{
			"shared":    map[string]any{"command": "user-shared"},
			"only-user": map[string]any{"command": "user-only"},
			"both":      map[string]any{"command": "user-both"},
		},
		"projects": map[string]any{
			cwd: map[string]any{
				"mcpServers": map[string]any{
					"shared":     map[string]any{"command": "local-shared"},
					"only-local": map[string]any{"command": "local-only"},
					"both":       map[string]any{"command": "local-both"},
				},
			},
		},
	})

	projectMCPPath := filepath.Join(cwd, ".mcp.json")
	writeJSON(t, projectMCPPath, map[string]any{
		"mcpServers": map[string]any{
			"both":      map[string]any{"command": "file-both"},
			"only-file": map[string]any{"command": "file-only"},
		},
	})

	s := &Scanner{
		ClaudeJSONPath: claudeJSONPath,
		ProjectMCPPath: projectMCPPath,
		CWD:            cwd,
	}
	entries, err := s.Scan()
	if err != nil {
		t.Fatal(err)
	}

	bySource := map[string]Entry{}
	for _, e := range entries {
		bySource[e.Name] = e
	}

	checks := []struct {
		name        string
		wantSource  Source
		wantCommand string
	}{
		{"both", SourceProjectFile, "file-both"},
		{"only-file", SourceProjectFile, "file-only"},
		{"only-local", SourceProjectLocal, "local-only"},
		{"only-user", SourceUserScope, "user-only"},
		{"shared", SourceProjectLocal, "local-shared"},
	}
	if len(entries) != len(checks) {
		t.Fatalf("entries = %d, want %d (entries=%+v)", len(entries), len(checks), entries)
	}
	for _, c := range checks {
		got, ok := bySource[c.name]
		if !ok {
			t.Errorf("missing entry %q", c.name)
			continue
		}
		if got.Source != c.wantSource {
			t.Errorf("%s: source = %s, want %s", c.name, got.Source, c.wantSource)
		}
		if got.Server.Command != c.wantCommand {
			t.Errorf("%s: command = %s, want %s", c.name, got.Server.Command, c.wantCommand)
		}
	}
}

func TestScan_MissingFilesIsEmpty(t *testing.T) {
	dir := t.TempDir()
	s := &Scanner{
		ClaudeJSONPath: filepath.Join(dir, ".claude.json"),
		ProjectMCPPath: filepath.Join(dir, ".mcp.json"),
		CWD:            dir,
	}
	entries, err := s.Scan()
	if err != nil {
		t.Fatalf("scan with missing files: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestHasGenieUserScope_OnlyTrueForUserScope(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "proj")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	// Genie present in project-file only — HasGenieUserScope should
	// still return false because project-file doesn't make genie
	// available in other projects.
	writeJSON(t, filepath.Join(cwd, ".mcp.json"), map[string]any{
		"mcpServers": map[string]any{
			"genie": map[string]any{"command": "/usr/local/bin/genie", "args": []string{"serve"}},
		},
	})
	s := &Scanner{
		ClaudeJSONPath: filepath.Join(dir, ".claude.json"),
		ProjectMCPPath: filepath.Join(cwd, ".mcp.json"),
		CWD:            cwd,
	}
	has, err := s.HasGenieUserScope()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasGenieUserScope = true (project-file only), want false")
	}

	// Now add genie to user-scope.
	writeJSON(t, s.ClaudeJSONPath, map[string]any{
		"mcpServers": map[string]any{
			"genie": map[string]any{"command": "/opt/genie/genie", "args": []string{"serve"}},
		},
	})
	has, err = s.HasGenieUserScope()
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("HasGenieUserScope = false (user-scope present), want true")
	}
}

func TestHasGenieUserScope_NoClaudeJSON(t *testing.T) {
	dir := t.TempDir()
	s := &Scanner{
		ClaudeJSONPath: filepath.Join(dir, ".claude.json"),
		ProjectMCPPath: filepath.Join(dir, ".mcp.json"),
		CWD:            dir,
	}
	has, err := s.HasGenieUserScope()
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("HasGenieUserScope = true (no file), want false")
	}
}

func TestWriteUserScopeMCPServer_PreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	claudeJSON := filepath.Join(dir, ".claude.json")
	writeJSON(t, claudeJSON, map[string]any{
		"mcpServers": map[string]any{
			"existing": map[string]any{
				"command":   "existing-bin",
				"args":      []string{"--flag"},
				"_artifact": "managed-by-claude",
			},
		},
		"projects": map[string]any{
			"/some/project": map[string]any{"mcpServers": map[string]any{}},
		},
		"someOtherKey": "preserve-me",
		"numericKey":   42,
		"arrayKey":     []string{"a", "b"},
	})

	s := &Scanner{ClaudeJSONPath: claudeJSON, CWD: dir}
	if err := s.WriteUserScopeMCPServer("genie", MCPServer{Command: "/path/to/genie", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(claudeJSON)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}

	if got := top["someOtherKey"]; got != "preserve-me" {
		t.Errorf("someOtherKey lost: %v", got)
	}
	if got := top["numericKey"]; got != float64(42) {
		t.Errorf("numericKey lost: %v", got)
	}
	if _, ok := top["projects"]; !ok {
		t.Error("projects key lost")
	}
	if _, ok := top["arrayKey"]; !ok {
		t.Error("arrayKey lost")
	}

	servers := top["mcpServers"].(map[string]any)
	existing := servers["existing"].(map[string]any)
	if existing["_artifact"] != "managed-by-claude" {
		t.Errorf("neighbour entry's _artifact field lost: %v", existing)
	}
	g := servers["genie"].(map[string]any)
	if g["command"] != "/path/to/genie" {
		t.Errorf("genie command = %v", g["command"])
	}
}

func TestWriteUserScopeMCPServer_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	claudeJSON := filepath.Join(dir, ".claude.json")
	s := &Scanner{ClaudeJSONPath: claudeJSON, CWD: dir}
	if err := s.WriteUserScopeMCPServer("genie", MCPServer{Command: "/g", Args: []string{"serve"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(claudeJSON); err != nil {
		t.Fatalf("file not created: %v", err)
	}
	raw, _ := os.ReadFile(claudeJSON)
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	g := top["mcpServers"].(map[string]any)["genie"].(map[string]any)
	if g["command"] != "/g" {
		t.Errorf("genie command = %v", g["command"])
	}
}

func TestDeleteUserScopeMCPServer_PreservesNeighbours(t *testing.T) {
	dir := t.TempDir()
	claudeJSON := filepath.Join(dir, ".claude.json")
	writeJSON(t, claudeJSON, map[string]any{
		"mcpServers": map[string]any{
			"granola":    map[string]any{"url": "https://granola", "_artifact": "kept"},
			"playwright": map[string]any{"command": "npx", "args": []string{"@playwright/mcp@latest"}},
		},
		"someOtherKey": "preserve-me",
	})
	s := &Scanner{ClaudeJSONPath: claudeJSON, CWD: dir}
	if err := s.DeleteUserScopeMCPServer("granola"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(claudeJSON)
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	servers := top["mcpServers"].(map[string]any)
	if _, ok := servers["granola"]; ok {
		t.Error("granola not deleted")
	}
	pw, ok := servers["playwright"].(map[string]any)
	if !ok || pw["command"] != "npx" {
		t.Errorf("playwright disturbed: %v", servers["playwright"])
	}
	if top["someOtherKey"] != "preserve-me" {
		t.Error("someOtherKey lost")
	}
}

func TestDeleteUserScopeMCPServer_MissingIsNoOp(t *testing.T) {
	dir := t.TempDir()
	claudeJSON := filepath.Join(dir, ".claude.json")
	writeJSON(t, claudeJSON, map[string]any{
		"mcpServers": map[string]any{
			"playwright": map[string]any{"command": "npx"},
		},
	})
	s := &Scanner{ClaudeJSONPath: claudeJSON, CWD: dir}
	if err := s.DeleteUserScopeMCPServer("nonexistent"); err != nil {
		t.Fatalf("delete missing should be no-op, got: %v", err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}
