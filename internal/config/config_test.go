package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_BasicProvider(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_xyz")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := writeFile(path, `{
		"mcpServers": {
			"github": {
				"command": "github-mcp-server",
				"args": ["stdio"],
				"env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "${env:GITHUB_TOKEN}" },
				"description": "GitHub"
			}
		}
	}`); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	gh, ok := cfg.MCPServers["github"]
	if !ok {
		t.Fatal("github provider missing")
	}
	if gh.Command != "github-mcp-server" {
		t.Errorf("command = %q", gh.Command)
	}
	if got := gh.Env["GITHUB_PERSONAL_ACCESS_TOKEN"]; got != "ghp_xyz" {
		t.Errorf("env expansion = %q, want %q", got, "ghp_xyz")
	}
}

func TestLoad_UnsetEnvIsHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	_ = writeFile(path, `{
		"mcpServers": {
			"x": {
				"command": "foo",
				"env": { "TOKEN": "${env:DOES_NOT_EXIST_GENIE_TEST}" }
			}
		}
	}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unset env var")
	}
	if !strings.Contains(err.Error(), "DOES_NOT_EXIST_GENIE_TEST") {
		t.Errorf("error should name missing var, got: %v", err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/genie-config.json")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !strings.Contains(err.Error(), "config not found") {
		t.Errorf("error should mention missing config, got: %v", err)
	}
}

func TestLoad_EmptyMcpServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	_ = writeFile(path, `{"mcpServers": {}}`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for empty mcpServers")
	}
}

func TestResolvePath_Override(t *testing.T) {
	got, err := ResolvePath("/explicit/path.json")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/explicit/path.json" {
		t.Errorf("override should win: got %q", got)
	}
}

func TestResolvePath_EnvVar(t *testing.T) {
	t.Setenv(PathEnvVar, "/from/env.json")
	got, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/from/env.json" {
		t.Errorf("env var should win when no override: got %q", got)
	}
}

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o644)
}
