// Package providers owns the lifecycle of upstream MCP server
// connections — both stdio subprocesses and HTTP/SSE remotes. A
// Registry is built once at startup from the parsed config; it
// connects to each provider eagerly and keeps the resulting
// mcpclient.Client around for the duration of the Genie process.
//
// A failed provider is logged and dropped, not fatal — so a
// misconfigured Linear server cannot prevent GitHub from working.
//
// HTTP providers may require OAuth. The first connect that
// receives an OAuthAuthorizationRequiredError triggers an
// interactive auth flow (browser + local callback); on success the
// retry succeeds. Tokens are persisted via auth.Vault so subsequent
// process starts skip the flow until the token expires (refresh is
// automatic via mcp-go) or is revoked.
package providers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/sleuth-io/genie/internal/auth"
	"github.com/sleuth-io/genie/internal/config"
	"github.com/sleuth-io/genie/internal/mcpclient"
)

// Info is what list_providers returns: tiny, no tool catalogs.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// Registry holds the live MCP clients for each configured provider.
// Get / List are safe for concurrent use; Close must be called from one
// goroutine, after no further calls are in flight.
type Registry struct {
	mu      sync.RWMutex
	clients map[string]*mcpclient.Client
	info    map[string]Info
	vault   auth.Vault
	cfg     *config.Config
}

// NewRegistry connects to one MCP server per configured provider and
// returns a Registry containing the successful ones. Failed connects
// are logged at warn level; they do not abort startup. The returned
// Registry is empty (and an error) only if every provider failed.
func NewRegistry(ctx context.Context, cfg *config.Config) (*Registry, error) {
	r := &Registry{
		clients: make(map[string]*mcpclient.Client, len(cfg.MCPServers)),
		info:    make(map[string]Info, len(cfg.MCPServers)),
		vault:   auth.Open(),
		cfg:     cfg,
	}

	var failures []string
	for name, prov := range cfg.MCPServers {
		c, err := r.connect(ctx, name, prov)
		if err != nil {
			slog.Warn("provider connect failed; skipping", "provider", name, "err", err)
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		r.clients[name] = c
		r.info[name] = Info{Name: name, Description: prov.Description}
		slog.Info("provider ready",
			"provider", name,
			"transport", prov.TransportType(),
			"tools", len(c.Tools()))
	}

	if len(r.clients) == 0 {
		return nil, fmt.Errorf("no providers started successfully (%d configured): %v",
			len(cfg.MCPServers), failures)
	}
	return r, nil
}

// connect builds the mcpclient.ProviderSpec for a configured server,
// runs the OAuth flow if the first connect requires authorization,
// and returns the live client.
func (r *Registry) connect(ctx context.Context, name string, prov config.ProviderConfig) (*mcpclient.Client, error) {
	spec := r.buildSpec(name, prov)
	c, err := mcpclient.Open(ctx, spec)
	if err == nil {
		return c, nil
	}
	var oauthErr *mcpclient.OAuthRequiredError
	if !errors.As(err, &oauthErr) {
		return nil, err
	}

	slog.Info("provider needs authorization; running OAuth flow", "provider", name)
	if err := auth.Run(ctx, auth.FlowConfig{
		ProviderName: name,
		ServerURL:    prov.URL,
		Scopes:       prov.Scopes,
		Vault:        r.vault,
	}); err != nil {
		return nil, fmt.Errorf("oauth flow: %w", err)
	}

	// Reload state so the retry picks up the freshly-registered
	// client_id + token.
	spec = r.buildSpec(name, prov)
	return mcpclient.Open(ctx, spec)
}

// buildSpec assembles a ProviderSpec from config + persisted auth
// state. Stdio providers ignore all OAuth fields.
func (r *Registry) buildSpec(name string, prov config.ProviderConfig) mcpclient.ProviderSpec {
	spec := mcpclient.ProviderSpec{
		Name:    name,
		Command: prov.Command,
		Args:    prov.Args,
		Env:     prov.Env,
		URL:     prov.URL,
		Type:    prov.TransportType(),
		Scopes:  prov.Scopes,
		Headers: prov.Headers,
	}
	if prov.IsHTTP() {
		spec.OAuthTokenStore = auth.NewTokenStore(r.vault, name)
		if state, err := r.vault.Load(name); err == nil {
			spec.OAuthClientID = state.ClientID
			spec.OAuthClientSecret = state.ClientSecret
			spec.OAuthRedirectURI = state.RedirectURI
		}
	}
	return spec
}

// Get returns the live MCP client for a provider name, or false if no
// such provider is registered (either unknown or failed at startup).
func (r *Registry) Get(name string) (*mcpclient.Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.clients[name]
	return c, ok
}

// List returns the registered providers' Info entries, sorted by name
// for deterministic output (so list_providers responses are stable).
func (r *Registry) List() []Info {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Info, 0, len(r.info))
	for _, i := range r.info {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Names returns the registered provider names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.clients))
	for n := range r.clients {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Close shuts down every provider's subprocess. Errors are logged but
// not returned; a single failed Close should not block the others.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, c := range r.clients {
		if err := c.Close(); err != nil {
			slog.Warn("provider close failed", "provider", name, "err", err)
		}
	}
	r.clients = nil
	return nil
}
