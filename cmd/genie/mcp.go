package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sleuth-io/genie/internal/auth"
	"github.com/sleuth-io/genie/internal/config"
)

// runMCP implements `genie mcp <subcommand>`, the config-edit surface
// users interact with instead of editing config.json by hand.
//
//	genie mcp add --name NAME (--url URL | --command "CMD [ARGS...]")
//	genie mcp add --json '{"name": "...", "url": "..."}'
//	genie mcp remove NAME
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
	jsonStr := fs.String("json", "", "register a provider from a JSON object")

	var name, urlFlag, commandFlag string
	fs.StringVar(&name, "name", "", "provider name (required unless using --json)")
	fs.StringVar(&name, "n", "", "shorthand for --name")
	fs.StringVar(&urlFlag, "url", "", "HTTP/SSE provider URL")
	fs.StringVar(&urlFlag, "u", "", "shorthand for --url")
	fs.StringVar(&commandFlag, "command", "", "stdio provider command (with args, whitespace-split)")
	fs.StringVar(&commandFlag, "c", "", "shorthand for --command")

	transportType := fs.String("type", "", "force transport: http|sse|stdio (default: inferred)")
	description := fs.String("description", "", "human-readable description (shown in list_providers)")
	noAuth := fs.Bool("no-auth", false, "skip the browser OAuth flow after adding (run `genie auth <name>` later)")
	force := fs.Bool("force", false, "overwrite an existing provider with the same name")
	clientID := fs.String("client-id", "", "OAuth client ID (use when the server doesn't support dynamic registration, e.g. Slack)")
	clientSecret := fs.String("client-secret", "", "OAuth client secret for confidential clients")
	var envFlags, headerFlags, scopeFlags stringList
	fs.Var(&envFlags, "env", "child-process env var, KEY=VALUE (repeatable)")
	fs.Var(&headerFlags, "header", "HTTP header, KEY=VALUE (repeatable)")
	fs.Var(&scopeFlags, "scope", "OAuth scope to request (repeatable)")
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
	case urlFlag != "" && commandFlag != "":
		return errors.New("--url and --command are mutually exclusive")
	case urlFlag != "":
		if name == "" {
			return errors.New("--name is required")
		}
		prov = config.ProviderConfig{URL: urlFlag}
	case commandFlag != "":
		if name == "" {
			return errors.New("--name is required")
		}
		parts := strings.Fields(commandFlag)
		prov = config.ProviderConfig{Command: parts[0], Args: parts[1:]}
	default:
		return errors.New(`usage: genie mcp add --name NAME (--url URL | --command "CMD [ARGS...]")
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

	// Run the OAuth flow BEFORE saving the config. A live `genie
	// serve` watches config.json and reloads on change; if we saved
	// first, the reload would race the flow (both processes pop a
	// browser, both wait on the callback). Auth-then-save means the
	// reload sees a config entry whose token is already in the
	// vault and connects without a second flow.
	if prov.IsHTTP() && !*noAuth && len(prov.Headers) == 0 {
		fmt.Fprintf(os.Stderr, "Authorizing %q…\n", name)
		if err := auth.Run(ctx, auth.FlowConfig{
			ProviderName: name,
			ServerURL:    prov.URL,
			Scopes:       prov.Scopes,
			ClientID:     *clientID,
			ClientSecret: *clientSecret,
			Vault:        auth.Open(),
		}); err != nil {
			return fmt.Errorf("auth flow: %w\n(config not saved; rerun `genie mcp add` to retry)", err)
		}
	}

	cfg.MCPServers[name] = prov
	if err := config.Save(path, cfg); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ Added provider %q (%s) to %s\n", name, prov.TransportType(), path)
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
		return errors.New(`usage: genie mcp remove NAME`)
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
		fmt.Printf("No providers configured. Add one with `genie mcp add --name NAME --url URL`.\n")
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
