// Package config loads Genie's provider configuration from a JSON file
// shaped like Claude Code's .mcp.json. Each entry under mcpServers
// describes one upstream MCP server Genie should spawn and front.
//
// Example:
//
//	{
//	  "mcpServers": {
//	    "github": {
//	      "command": "github-mcp-server",
//	      "args": ["stdio"],
//	      "env": { "GITHUB_PERSONAL_ACCESS_TOKEN": "${env:GITHUB_TOKEN}" },
//	      "description": "GitHub repos, PRs, issues"
//	    }
//	  }
//	}
//
// The ${env:VAR} syntax interpolates from the surrounding process
// environment at load time. Unset variables are a hard error so a typo
// in the config does not silently spawn an MCP server with an empty
// auth token.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// PathEnvVar lets callers point the loader at an alternate config path,
// equivalent to passing --config <path>.
const PathEnvVar = "GENIE_CONFIG"

// ProviderConfig is one entry under mcpServers — how Genie reaches an
// upstream MCP server. Two transport shapes:
//
//   - stdio:    Command + Args + Env (subprocess piping stdio)
//   - http/sse: URL + optional Type + optional Scopes + optional Headers
//
// Type defaults to "http" when URL is set; pass "sse" to force the SSE
// transport. Scopes is the OAuth scope list to request when the server
// requires authorization. Headers are static request headers (e.g. for
// pre-issued API keys); not used for OAuth, the OAuth handler inserts
// the Authorization header itself.
//
// Description is Genie-specific (surfaced by list_providers).
type ProviderConfig struct {
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Type        string            `json:"type,omitempty"`
	Scopes      []string          `json:"scopes,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Description string            `json:"description,omitempty"`
}

// IsHTTP reports whether the provider uses an HTTP-style transport
// (http or sse).
func (p ProviderConfig) IsHTTP() bool {
	return p.URL != ""
}

// TransportType returns the resolved transport: "stdio", "http", or "sse".
func (p ProviderConfig) TransportType() string {
	if p.URL == "" {
		return "stdio"
	}
	if p.Type == "sse" {
		return "sse"
	}
	return "http"
}

// Config is the parsed config file.
type Config struct {
	MCPServers map[string]ProviderConfig `json:"mcpServers"`
}

// ResolvePath returns the config file path Genie should load, applying
// the precedence: explicit override > GENIE_CONFIG env var > default
// location under the user's config directory. The returned path is not
// guaranteed to exist; callers should handle os.IsNotExist.
func ResolvePath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if v := os.Getenv(PathEnvVar); v != "" {
		return v, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config dir: %w", err)
	}
	return filepath.Join(dir, "genie", "config.json"), nil
}

// Load reads and parses the config at path, then expands ${env:VAR}
// references in env values. A missing config file produces a friendly
// error suggesting a starter config; malformed JSON or unset env-var
// references are hard failures.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config not found at %s\n\n"+
				"Create one with at least one provider, e.g.:\n\n"+
				"  {\n"+
				"    \"mcpServers\": {\n"+
				"      \"github\": {\n"+
				"        \"command\": \"github-mcp-server\",\n"+
				"        \"args\": [\"stdio\"],\n"+
				"        \"env\": {\"GITHUB_PERSONAL_ACCESS_TOKEN\": \"${env:GITHUB_TOKEN}\"}\n"+
				"      }\n"+
				"    }\n"+
				"  }", path)
		}
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if len(cfg.MCPServers) == 0 {
		return nil, fmt.Errorf("config %q: no providers defined under mcpServers", path)
	}

	for name, prov := range cfg.MCPServers {
		hasCmd := strings.TrimSpace(prov.Command) != ""
		hasURL := strings.TrimSpace(prov.URL) != ""
		switch {
		case hasCmd && hasURL:
			return nil, fmt.Errorf("config %q: provider %q: set either command or url, not both", path, name)
		case !hasCmd && !hasURL:
			return nil, fmt.Errorf("config %q: provider %q: one of command or url is required", path, name)
		}
		if prov.Type != "" && prov.Type != "stdio" && prov.Type != "http" && prov.Type != "sse" {
			return nil, fmt.Errorf("config %q: provider %q: type %q not recognised (want stdio|http|sse)", path, name, prov.Type)
		}
		if prov.Env != nil {
			expanded, err := expandEnvMap(prov.Env, name)
			if err != nil {
				return nil, fmt.Errorf("config %q: %w", path, err)
			}
			prov.Env = expanded
		}
		if prov.Headers != nil {
			expanded, err := expandEnvMap(prov.Headers, name)
			if err != nil {
				return nil, fmt.Errorf("config %q: %w", path, err)
			}
			prov.Headers = expanded
		}
		cfg.MCPServers[name] = prov
	}
	return &cfg, nil
}

// LoadForEdit reads the config without expanding ${env:VAR} references
// or validating providers. Use this when you intend to mutate and save
// the config back (genie mcp add/remove); Load is destructive in that
// context because it inlines env-var values.
//
// Returns an empty Config (zero entries under mcpServers) if the file
// does not exist, so callers can treat "first edit" identically to
// "subsequent edit".
func LoadForEdit(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{MCPServers: map[string]ProviderConfig{}}, nil
		}
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ProviderConfig{}
	}
	return &cfg, nil
}

// Save writes the config to path with stable indentation. Directory
// is created (0700) if missing; file is written 0600 because it can
// hold tokens in env values. Atomic via tempfile + rename.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(buf, '\n'), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

var envRefPattern = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnvMap rewrites ${env:VAR} references in each value against the
// process environment. Unset variables are a hard error so a typo does
// not silently spawn an MCP server with an empty token.
func expandEnvMap(in map[string]string, providerName string) (map[string]string, error) {
	out := make(map[string]string, len(in))
	for k, v := range in {
		matches := envRefPattern.FindAllStringSubmatchIndex(v, -1)
		if len(matches) == 0 {
			out[k] = v
			continue
		}
		var b strings.Builder
		last := 0
		for _, m := range matches {
			b.WriteString(v[last:m[0]])
			varName := v[m[2]:m[3]]
			val, ok := os.LookupEnv(varName)
			if !ok {
				return nil, fmt.Errorf("provider %q env %q references ${env:%s} but %s is not set",
					providerName, k, varName, varName)
			}
			b.WriteString(val)
			last = m[1]
		}
		b.WriteString(v[last:])
		out[k] = b.String()
	}
	return out, nil
}
