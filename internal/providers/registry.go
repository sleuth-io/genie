// Package providers owns the lifecycle of upstream MCP server
// subprocesses. A Registry is built once at startup from the parsed
// config; it spawns each provider eagerly and keeps the resulting
// mcpclient.Client around for the duration of the Genie process. A
// failed provider is logged and dropped, not fatal — so a misconfigured
// Linear server cannot prevent GitHub from working.
package providers

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"

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
}

// NewRegistry spawns one MCP client per configured provider and returns
// a Registry containing the successful ones. Failed spawns are logged
// at warn level; they do not abort startup. The returned Registry is
// empty (and an error) only if every provider failed.
func NewRegistry(ctx context.Context, cfg *config.Config) (*Registry, error) {
	r := &Registry{
		clients: make(map[string]*mcpclient.Client, len(cfg.MCPServers)),
		info:    make(map[string]Info, len(cfg.MCPServers)),
	}

	var failures []string
	for name, prov := range cfg.MCPServers {
		spec := mcpclient.ProviderSpec{
			Name:    name,
			Command: prov.Command,
			Args:    prov.Args,
			Env:     prov.Env,
		}
		c, err := mcpclient.Open(ctx, spec)
		if err != nil {
			slog.Warn("provider spawn failed; skipping", "provider", name, "err", err)
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		r.clients[name] = c
		r.info[name] = Info{Name: name, Description: prov.Description}
		slog.Info("provider ready", "provider", name, "tools", len(c.Tools()))
	}

	if len(r.clients) == 0 {
		return nil, fmt.Errorf("no providers started successfully (%d configured): %v",
			len(cfg.MCPServers), failures)
	}
	return r, nil
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
