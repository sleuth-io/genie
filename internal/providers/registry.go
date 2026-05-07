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
	mu       sync.RWMutex
	clients  map[string]*mcpclient.Client
	info     map[string]Info
	entries  map[string]config.ProviderConfig
	vault    auth.Vault
	reloadMu sync.Mutex // serialises Reload calls
}

// NewRegistry connects to one MCP server per configured provider and
// returns a Registry containing the successful ones. Failed connects
// are logged at warn level; they do not abort startup. Zero
// configured providers is fine — `genie serve` starts up empty so
// the user can `genie mcp add` later (after a serve restart). Zero
// successful connects from a non-empty config is also fine; the
// failures are surfaced in the log.
func NewRegistry(ctx context.Context, cfg *config.Config) (*Registry, error) {
	r := &Registry{
		clients: make(map[string]*mcpclient.Client, len(cfg.MCPServers)),
		info:    make(map[string]Info, len(cfg.MCPServers)),
		entries: make(map[string]config.ProviderConfig, len(cfg.MCPServers)),
		vault:   auth.Open(),
	}

	for name, prov := range cfg.MCPServers {
		c, err := r.connect(ctx, name, prov)
		if err != nil {
			slog.Warn("provider connect failed; skipping", "provider", name, "err", err)
			continue
		}
		r.clients[name] = c
		r.info[name] = Info{Name: name, Description: prov.Description}
		r.entries[name] = prov
		slog.Info("provider ready",
			"provider", name,
			"transport", prov.TransportType(),
			"tools", len(c.Tools()))
	}
	return r, nil
}

// Reload diffs newCfg against the current registry state and applies
// the delta: open new providers, close removed ones, replace ones
// whose config changed. Connections happen outside the registry lock
// (an OAuth flow can take minutes); only the swap is exclusive.
//
// Reload calls are serialised against each other but not against
// Get/List/Names/Close. In-flight queries against a provider that's
// being replaced or removed retain their *mcpclient.Client reference;
// Close fires after they return (the registry's contract is "Close
// when no further calls are in flight against the closing provider"
// — for replace+remove, Reload waits until the swap before closing,
// so a query that picked up the old client just before the swap
// keeps working until completion).
func (r *Registry) Reload(ctx context.Context, newCfg *config.Config) error {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	r.mu.RLock()
	currentEntries := make(map[string]config.ProviderConfig, len(r.entries))
	for k, v := range r.entries {
		currentEntries[k] = v
	}
	r.mu.RUnlock()

	toAdd, toRemove, toReplace := diffEntries(currentEntries, newCfg.MCPServers)
	if len(toAdd) == 0 && len(toRemove) == 0 && len(toReplace) == 0 {
		return nil
	}

	// Open new connections OUTSIDE the lock — OAuth flows can block
	// for minutes; meanwhile Get/List remain available.
	connected := make(map[string]*mcpclient.Client)
	for name, prov := range toAdd {
		c, err := r.connect(ctx, name, prov)
		if err != nil {
			slog.Warn("reload: provider connect failed; skipping", "provider", name, "err", err)
			continue
		}
		connected[name] = c
		slog.Info("reload: provider added",
			"provider", name,
			"transport", prov.TransportType(),
			"tools", len(c.Tools()))
	}
	for name, prov := range toReplace {
		c, err := r.connect(ctx, name, prov)
		if err != nil {
			slog.Warn("reload: provider reconnect failed; keeping previous", "provider", name, "err", err)
			delete(toReplace, name)
			continue
		}
		connected[name] = c
		slog.Info("reload: provider replaced",
			"provider", name,
			"transport", prov.TransportType(),
			"tools", len(c.Tools()))
	}

	// Swap state and collect old clients for deferred close.
	var toClose []*mcpclient.Client
	r.mu.Lock()
	for name := range toRemove {
		if c, ok := r.clients[name]; ok {
			toClose = append(toClose, c)
		}
		delete(r.clients, name)
		delete(r.info, name)
		delete(r.entries, name)
		slog.Info("reload: provider removed", "provider", name)
	}
	for name := range toReplace {
		if c, ok := r.clients[name]; ok {
			toClose = append(toClose, c)
		}
	}
	for name, c := range connected {
		prov := newCfg.MCPServers[name]
		r.clients[name] = c
		r.info[name] = Info{Name: name, Description: prov.Description}
		r.entries[name] = prov
	}
	r.mu.Unlock()

	for _, c := range toClose {
		if err := c.Close(); err != nil {
			slog.Warn("reload: close old client failed", "err", err)
		}
	}
	return nil
}

// diffEntries returns (added, removed, replaced) maps. A name is
// "replaced" when both maps contain it but the entries differ.
func diffEntries(old, next map[string]config.ProviderConfig) (added, removed, replaced map[string]config.ProviderConfig) {
	added = make(map[string]config.ProviderConfig)
	removed = make(map[string]config.ProviderConfig)
	replaced = make(map[string]config.ProviderConfig)
	for name, prov := range next {
		if cur, ok := old[name]; !ok {
			added[name] = prov
		} else if !sameEntry(cur, prov) {
			replaced[name] = prov
		}
	}
	for name, prov := range old {
		if _, ok := next[name]; !ok {
			removed[name] = prov
		}
	}
	return
}

func sameEntry(a, b config.ProviderConfig) bool {
	if a.Command != b.Command || a.URL != b.URL || a.Type != b.Type || a.Description != b.Description {
		return false
	}
	if !equalStringSlice(a.Args, b.Args) || !equalStringSlice(a.Scopes, b.Scopes) {
		return false
	}
	return equalStringMap(a.Env, b.Env) && equalStringMap(a.Headers, b.Headers)
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
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
