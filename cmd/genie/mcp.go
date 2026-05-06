package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/sleuth-io/genie/internal/auth"
	"github.com/sleuth-io/genie/internal/config"
)

// runMCP implements `genie mcp <subcommand>`, the config-edit surface
// users interact with instead of editing config.json by hand.
//
//	genie mcp add <name> <url|command> [args...]
//	genie mcp add --json '<json>'
//	genie mcp remove <name>
//	genie mcp list
//
// `add` for an HTTP URL automatically runs the OAuth flow once the
// entry is saved, so adding an MCP server is a single command end to
// end.
func runMCP(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New(`usage: genie mcp [add|remove|list] [args]`)
	}
	switch args[0] {
	case "add":
		return runMCPAdd(ctx, args[1:])
	case "remove", "rm":
		return runMCPRemove(args[1:])
	case "list", "ls":
		return runMCPList(args[1:])
	default:
		return fmt.Errorf("unknown mcp subcommand %q", args[0])
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func runMCPAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp add", flag.ContinueOnError)
	configPath := fs.String("config", "", "override config path")
	jsonStr := fs.String("json", "", "register a provider from a JSON object (mutually exclusive with positional args)")
	transportType := fs.String("type", "", "force transport: http|sse|stdio (default: inferred)")
	description := fs.String("description", "", "human-readable description (shown in list_providers)")
	noAuth := fs.Bool("no-auth", false, "skip the browser OAuth flow after adding (run `genie auth <name>` later)")
	force := fs.Bool("force", false, "overwrite an existing provider with the same name")
	var envFlags, headerFlags, scopeFlags stringList
	fs.Var(&envFlags, "env", "child-process env var, KEY=VALUE (repeatable)")
	fs.Var(&headerFlags, "header", "HTTP header, KEY=VALUE (repeatable)")
	fs.Var(&scopeFlags, "scope", "OAuth scope to request (repeatable)")

	// Allow flags to follow positionals (e.g. `genie mcp add github
	// github-mcp-server stdio --env KEY=VAL`) by splitting at the
	// first arg that starts with "-". Subprocess command args that
	// start with "-" must be quoted under a `--` separator.
	positional, flagArgs := splitPositional(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	rest := positional

	path, err := config.ResolvePath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadForEdit(path)
	if err != nil {
		return err
	}

	var name string
	var prov config.ProviderConfig

	switch {
	case *jsonStr != "":
		// `genie mcp add --json '{"name": "linear", "url": "..."}'`
		var raw struct {
			Name string `json:"name"`
			config.ProviderConfig
		}
		if err := json.Unmarshal([]byte(*jsonStr), &raw); err != nil {
			return fmt.Errorf("parse --json: %w", err)
		}
		if raw.Name == "" {
			return errors.New("--json: missing required field `name`")
		}
		name = raw.Name
		prov = raw.ProviderConfig
	case len(rest) >= 2:
		name = rest[0]
		target := rest[1]
		if looksLikeURL(target) {
			prov = config.ProviderConfig{URL: target}
		} else {
			prov = config.ProviderConfig{Command: target, Args: rest[2:]}
		}
	default:
		return errors.New(`usage: genie mcp add <name> <url|command> [args...]
       genie mcp add --json '{"name": "...", "url": "..."}'`)
	}

	if *transportType != "" {
		prov.Type = *transportType
	}
	if *description != "" {
		prov.Description = *description
	}
	if len(envFlags) > 0 {
		prov.Env = parseKVPairs(envFlags, "--env")
	}
	if len(headerFlags) > 0 {
		prov.Headers = parseKVPairs(headerFlags, "--header")
	}
	if len(scopeFlags) > 0 {
		prov.Scopes = []string(scopeFlags)
	}

	if err := validateProvider(name, prov); err != nil {
		return err
	}

	if _, exists := cfg.MCPServers[name]; exists && !*force {
		return fmt.Errorf("provider %q already exists; pass --force to overwrite", name)
	}
	cfg.MCPServers[name] = prov
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ Added provider %q (%s) to %s\n", name, prov.TransportType(), path)

	// For HTTP/SSE providers, immediately run the auth flow so a
	// single `mcp add` call ends with everything wired up. Skip if
	// --no-auth was passed (e.g. for non-OAuth HTTP servers fronted
	// by a static API key in headers).
	if prov.IsHTTP() && !*noAuth && len(prov.Headers) == 0 {
		fmt.Fprintf(os.Stderr, "Authorizing %q…\n", name)
		if err := auth.Run(ctx, auth.FlowConfig{
			ProviderName: name,
			ServerURL:    prov.URL,
			Scopes:       prov.Scopes,
			Vault:        auth.Open(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "  (auth flow failed: %v — run `genie auth %s` later to retry)\n", err, name)
		}
	}
	return nil
}

func runMCPRemove(args []string) error {
	fs := flag.NewFlagSet("mcp remove", flag.ContinueOnError)
	configPath := fs.String("config", "", "override config path")
	keepCreds := fs.Bool("keep-credentials", false, "leave OAuth credentials in the keyring (default: drop them)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return errors.New(`usage: genie mcp remove <name>`)
	}
	name := rest[0]

	path, err := config.ResolvePath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadForEdit(path)
	if err != nil {
		return err
	}
	if _, ok := cfg.MCPServers[name]; !ok {
		return fmt.Errorf("provider %q not in %s", name, path)
	}
	delete(cfg.MCPServers, name)
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ Removed provider %q from %s\n", name, path)

	if !*keepCreds {
		vault := auth.Open()
		if err := vault.Delete(name); err != nil {
			fmt.Fprintf(os.Stderr, "  (could not clear credentials: %v)\n", err)
		}
	}
	return nil
}

func runMCPList(args []string) error {
	fs := flag.NewFlagSet("mcp list", flag.ContinueOnError)
	configPath := fs.String("config", "", "override config path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path, err := config.ResolvePath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadForEdit(path)
	if err != nil {
		return err
	}
	if len(cfg.MCPServers) == 0 {
		fmt.Printf("No providers configured. Add one with `genie mcp add <name> <url|command>`.\n")
		return nil
	}

	names := make([]string, 0, len(cfg.MCPServers))
	for n := range cfg.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("%-20s %-10s %s\n", "NAME", "TRANSPORT", "TARGET")
	for _, n := range names {
		p := cfg.MCPServers[n]
		target := p.URL
		if target == "" {
			target = p.Command
			if len(p.Args) > 0 {
				target += " " + strings.Join(p.Args, " ")
			}
		}
		fmt.Printf("%-20s %-10s %s\n", n, p.TransportType(), target)
	}
	fmt.Printf("\nconfig: %s\n", path)
	return nil
}

// splitPositional separates positional args from flag args. Args
// starting with "-" are treated as the start of the flag block; the
// special token "--" terminates positionals explicitly so subprocess
// command args that legitimately start with "-" can survive.
func splitPositional(args []string) (positional, flagArgs []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
		if strings.HasPrefix(a, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

func looksLikeURL(s string) bool {
	if !strings.Contains(s, "://") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func parseKVPairs(in []string, flag string) map[string]string {
	out := make(map[string]string, len(in))
	for _, kv := range in {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			fmt.Fprintf(os.Stderr, "%s: ignoring malformed pair %q (want KEY=VALUE)\n", flag, kv)
			continue
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out
}

func validateProvider(name string, p config.ProviderConfig) error {
	if name == "" {
		return errors.New("provider name is required")
	}
	hasCmd := strings.TrimSpace(p.Command) != ""
	hasURL := strings.TrimSpace(p.URL) != ""
	if hasCmd && hasURL {
		return errors.New("provider has both command and url; pick one")
	}
	if !hasCmd && !hasURL {
		return errors.New("provider needs either a command (stdio) or a url (http/sse)")
	}
	if p.Type != "" && p.Type != "stdio" && p.Type != "http" && p.Type != "sse" {
		return fmt.Errorf("type %q not recognised (want stdio|http|sse)", p.Type)
	}
	return nil
}
