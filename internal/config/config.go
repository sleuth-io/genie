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

// ProviderConfig is one entry under mcpServers — how to spawn an upstream
// MCP server. Description is Genie-specific (used by list_providers); all
// other fields mirror Claude Code's .mcp.json format.
type ProviderConfig struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Description string            `json:"description,omitempty"`
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
		if strings.TrimSpace(prov.Command) == "" {
			return nil, fmt.Errorf("config %q: provider %q: command is required", path, name)
		}
		if prov.Env != nil {
			expanded, err := expandEnvMap(prov.Env, name)
			if err != nil {
				return nil, fmt.Errorf("config %q: %w", path, err)
			}
			prov.Env = expanded
			cfg.MCPServers[name] = prov
		}
	}
	return &cfg, nil
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
