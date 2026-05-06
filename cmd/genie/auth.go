package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/sleuth-io/genie/internal/auth"
	"github.com/sleuth-io/genie/internal/config"
)

// runAuth handles `genie auth <provider>` (force re-auth) and
// `genie auth list` (show status of stored credentials).
func runAuth(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("auth", flag.ContinueOnError)
	configPath := fs.String("config", "", "override config path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) < 1 {
		return errors.New(`usage: genie auth (list | <provider> | logout <provider>)`)
	}

	switch rest[0] {
	case "list":
		return authList(*configPath)
	case "logout":
		if len(rest) < 2 {
			return errors.New(`usage: genie auth logout <provider>`)
		}
		return authLogout(rest[1])
	default:
		return authLogin(ctx, *configPath, rest[0])
	}
}

func loadProviderConfig(configPath, name string) (config.ProviderConfig, error) {
	path, err := config.ResolvePath(configPath)
	if err != nil {
		return config.ProviderConfig{}, err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return config.ProviderConfig{}, err
	}
	prov, ok := cfg.MCPServers[name]
	if !ok {
		return config.ProviderConfig{}, fmt.Errorf("provider %q not in %s", name, path)
	}
	if !prov.IsHTTP() {
		return config.ProviderConfig{}, fmt.Errorf("provider %q uses stdio transport; OAuth applies only to http/sse providers", name)
	}
	return prov, nil
}

func authLogin(ctx context.Context, configPath, name string) error {
	prov, err := loadProviderConfig(configPath, name)
	if err != nil {
		return err
	}

	vault := auth.Open()
	// Drop any existing state so we re-register and re-authorize
	// from scratch. The user asked for a clean re-auth.
	if err := vault.Delete(name); err != nil {
		return fmt.Errorf("clear existing state: %w", err)
	}

	if err := auth.Run(ctx, auth.FlowConfig{
		ProviderName: name,
		ServerURL:    prov.URL,
		Scopes:       prov.Scopes,
		Vault:        vault,
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ Authenticated %q (storage: %s)\n", name, vault.Backend())
	return nil
}

func authLogout(name string) error {
	vault := auth.Open()
	if err := vault.Delete(name); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ Removed credentials for %q (storage: %s)\n", name, vault.Backend())
	return nil
}

func authList(configPath string) error {
	path, err := config.ResolvePath(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	vault := auth.Open()

	names := make([]string, 0, len(cfg.MCPServers))
	for n := range cfg.MCPServers {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("%-20s %-12s %-10s %s\n", "PROVIDER", "TRANSPORT", "AUTH", "EXPIRES")
	for _, name := range names {
		prov := cfg.MCPServers[name]
		transport := prov.TransportType()
		authState := "—"
		expires := "—"
		if prov.IsHTTP() {
			state, err := vault.Load(name)
			switch {
			case errors.Is(err, auth.ErrNotFound):
				authState = "not authenticated"
			case err != nil:
				authState = "error"
				expires = err.Error()
			case state.Token == nil:
				authState = "registered"
			default:
				authState = "ok"
				if !state.Token.ExpiresAt.IsZero() {
					expires = humanizeUntil(state.Token.ExpiresAt)
				} else {
					expires = "never"
				}
			}
		}
		fmt.Printf("%-20s %-12s %-10s %s\n", name, transport, authState, expires)
	}
	fmt.Printf("\nstorage backend: %s\n", vault.Backend())
	return nil
}

func humanizeUntil(t time.Time) string {
	d := time.Until(t)
	if d < 0 {
		return "expired " + (-d).Round(time.Minute).String() + " ago"
	}
	if d < 24*time.Hour {
		return "in " + d.Round(time.Minute).String()
	}
	return fmt.Sprintf("in %dd", int(d/(24*time.Hour)))
}
