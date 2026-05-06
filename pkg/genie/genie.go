// Package genie is the public Go API for embedding Genie inside an
// agent process. It wraps the resolution pipeline (MCP client → host
// functions → monty runtime → plan generator → executor) behind a small
// surface:
//
//	g, err := genie.New(ctx, genie.Config{
//	    Providers:    []genie.Provider{genie.GitHubMCP(token)},
//	    AnthropicKey: os.Getenv("ANTHROPIC_API_KEY"),
//	    CacheDir:     "/abs/path/to/cache",
//	})
//	defer g.Close()
//	out, err := g.Query(ctx, genie.QueryRequest{Provider: "github", Query: "..."})
//
// The same pipeline backs the `genie` CLI (Mode 3) and the `genie serve`
// MCP server (Mode 1) — they all call through this package, so the
// behaviour you see in one mode is the behaviour you get in the others.
package genie

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/sleuth-io/genie/internal/config"
	"github.com/sleuth-io/genie/internal/crystallize"
	"github.com/sleuth-io/genie/internal/engine"
	"github.com/sleuth-io/genie/internal/llm"
	"github.com/sleuth-io/genie/internal/mcpclient"
	"github.com/sleuth-io/genie/internal/plan"
	"github.com/sleuth-io/genie/internal/providers"
	"github.com/sleuth-io/genie/internal/runtime"
	"github.com/sleuth-io/genie/internal/sandbox"
)

// Config configures a Genie instance. Either Providers or ConfigPath
// is consulted (in that order); if both are empty the default config
// path is loaded.
type Config struct {
	// Providers lists the upstream MCP servers Genie should front. Use
	// the helper factories like GitHubMCP, or build a custom Provider.
	// Takes precedence over ConfigPath when set.
	Providers []Provider

	// ConfigPath points at a JSON config file in the same shape as
	// Claude Code's .mcp.json. If both Providers and ConfigPath are
	// empty, Genie loads from the default location (resolved via
	// internal/config.ResolvePath, typically
	// ~/.config/genie/config.json on Linux).
	ConfigPath string

	// AnthropicKey authenticates the plan-generation calls when the
	// Anthropic SDK backend is selected. If empty and no
	// ANTHROPIC_API_KEY env var is set, Genie falls back to the
	// Claude Code CLI subprocess (`claude`) if that binary is on PATH
	// — useful when running under Claude Code itself.
	AnthropicKey string

	// CacheDir is the root directory for the crystallized cache. Each
	// provider gets a subdirectory beneath it. If empty, Genie uses
	// $GENIE_CACHE_DIR or ~/.cache/genie/crystallized.
	CacheDir string
}

// Provider describes one upstream MCP server. Use one of the helper
// factories (e.g. GitHubMCP) or build your own.
type Provider struct {
	// Name is the routing key callers use in QueryRequest.Provider.
	Name string

	// Description shows up in list_providers output.
	Description string

	// Command is the executable Genie spawns.
	Command string

	// Args are the args passed to Command.
	Args []string

	// Env is the env-var map applied to the child process only.
	Env map[string]string
}

// GitHubMCP builds a Provider for the official github-mcp-server, using
// the supplied token for authentication.
func GitHubMCP(token string) Provider {
	return Provider{
		Name:        "github",
		Description: "GitHub repos, PRs, issues",
		Command:     mcpclient.GitHubBinary,
		Args:        []string{"stdio"},
		Env:         map[string]string{mcpclient.PATEnvVar: token},
	}
}

// QueryRequest is the input shape for Genie.Query.
type QueryRequest struct {
	// Provider names which configured upstream to route the query to.
	Provider string

	// Query is a GraphQL-shaped string. Field names may be invented;
	// they are treated as intent signals, not schema references.
	Query string
}

// Result is the output shape for Genie.Query. Data is the resolved
// response, shaped to match the requested field selection.
type Result struct {
	Data map[string]any
}

// ProviderInfo is what ListProviders returns.
type ProviderInfo = providers.Info

// Genie is the top-level instance. Construct with New, query with
// Query, and tear down with Close. Safe for concurrent use.
type Genie struct {
	registry  *providers.Registry
	monty     *runtime.MontyEngine
	llm       llm.Client
	cacheRoot string
	bundles   map[string]*providerBundle
}

// providerBundle is the per-provider engine state. Built once per
// provider in New so generator metrics persist across queries (the
// eval harness reads them before/after each case).
type providerBundle struct {
	client    *mcpclient.Client
	store     *crystallize.Store
	generator *plan.Generator
	executor  *engine.Executor
}

// New constructs a Genie from cfg. It eagerly spawns the configured
// providers (failed providers are logged and dropped — see Registry).
// The caller is responsible for calling Close.
func New(ctx context.Context, cfg Config) (*Genie, error) {
	cacheRoot, err := resolveCacheDir(cfg.CacheDir)
	if err != nil {
		return nil, err
	}

	internalCfg, err := buildInternalConfig(cfg)
	if err != nil {
		return nil, err
	}

	monty, err := runtime.NewMontyEngineOwned()
	if err != nil {
		return nil, fmt.Errorf("init monty engine: %w", err)
	}

	registry, err := providers.NewRegistry(ctx, internalCfg)
	if err != nil {
		_ = monty.Close()
		return nil, err
	}

	llmClient, backend, err := llm.Select(cfg.AnthropicKey)
	if err != nil {
		_ = registry.Close()
		_ = monty.Close()
		return nil, err
	}
	slog.Info("genie: llm backend selected", "backend", backend)

	bundles := make(map[string]*providerBundle, len(registry.Names()))
	for _, name := range registry.Names() {
		client, _ := registry.Get(name)
		bundles[name] = buildProviderBundle(name, client, monty, llmClient, cacheRoot)
	}

	return &Genie{
		registry:  registry,
		monty:     monty,
		llm:       llmClient,
		cacheRoot: cacheRoot,
		bundles:   bundles,
	}, nil
}

// Query resolves a GraphQL-shaped query against the named provider.
// First call for a query shape pays an LLM-generation cost; subsequent
// calls hit the crystallized cache.
func (g *Genie) Query(ctx context.Context, req QueryRequest) (*Result, error) {
	if req.Provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	if req.Query == "" {
		return nil, fmt.Errorf("query is required")
	}

	bundle, ok := g.bundles[req.Provider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (known: %v)", req.Provider, g.registry.Names())
	}

	parsed, err := engine.Parse(req.Query)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	out, err := bundle.executor.Execute(ctx, parsed)
	if err != nil {
		return nil, err
	}
	return &Result{Data: out}, nil
}

// QueryMap resolves a query and returns just the result map. Convenience
// wrapper for callers that don't need the Result envelope (cmd/genie's
// internal handlers, the eval harness, the MCP run_query handler).
func (g *Genie) QueryMap(ctx context.Context, provider, query string) (map[string]any, error) {
	res, err := g.Query(ctx, QueryRequest{Provider: provider, Query: query})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// ListProviders returns the registered providers' Info entries.
func (g *Genie) ListProviders() []ProviderInfo {
	return g.registry.List()
}

// ProviderNames returns just the registered provider names, sorted.
// Used by cmd/genie/serve to populate the run_query schema enum.
func (g *Genie) ProviderNames() []string {
	return g.registry.Names()
}

// Names is an alias for ProviderNames matching the mcpserver.ProviderLister
// interface.
func (g *Genie) Names() []string {
	return g.registry.Names()
}

// List is an alias for ListProviders matching mcpserver.ProviderLister.
func (g *Genie) List() []ProviderInfo {
	return g.registry.List()
}

// ExecutorFor returns the per-provider Executor. Internal callers like
// the eval harness need direct access; agent integrations should use
// Query instead.
func (g *Genie) ExecutorFor(provider string) (*engine.Executor, bool) {
	b, ok := g.bundles[provider]
	if !ok {
		return nil, false
	}
	return b.executor, true
}

// GeneratorFor returns the per-provider plan Generator (for the
// hypothesis-3 NormalizeOnly path used by the eval harness).
func (g *Genie) GeneratorFor(provider string) (*plan.Generator, bool) {
	b, ok := g.bundles[provider]
	if !ok {
		return nil, false
	}
	return b.generator, true
}

// Close shuts down every provider's subprocess and the monty runtime.
func (g *Genie) Close() error {
	if g == nil {
		return nil
	}
	if g.registry != nil {
		_ = g.registry.Close()
	}
	if g.monty != nil {
		return g.monty.Close()
	}
	return nil
}

func buildProviderBundle(name string, client *mcpclient.Client, monty *runtime.MontyEngine, llmClient llm.Client, cacheRoot string) *providerBundle {
	mcpFuncs, mcpParams := mcpclient.BuildHostFunctions(client)
	clockFuncs, clockParams := sandbox.BuildClockBuiltins()
	builtIns, params := sandbox.MergeBuiltins(
		struct {
			Funcs  map[string]runtime.GoFunc
			Params map[string][]string
		}{Funcs: mcpFuncs, Params: mcpParams},
		struct {
			Funcs  map[string]runtime.GoFunc
			Params map[string][]string
		}{Funcs: clockFuncs, Params: clockParams},
	)
	caps := &runtime.Capabilities{
		BuiltIns:      builtIns,
		BuiltInParams: params,
		Limits:        runtime.Limits{MaxDuration: 60 * time.Second},
	}

	store := crystallize.NewStore(filepath.Join(cacheRoot, name))
	gen := plan.NewGenerator(client, store, llmClient)
	ex := engine.NewExecutor(monty, caps, store).WithGenerator(gen)

	return &providerBundle{
		client:    client,
		store:     store,
		generator: gen,
		executor:  ex,
	}
}

// buildInternalConfig converts the public Config into the internal
// config.Config shape. If Providers are supplied, they win; else load
// from ConfigPath (or the default path).
func buildInternalConfig(cfg Config) (*config.Config, error) {
	if len(cfg.Providers) > 0 {
		out := &config.Config{MCPServers: make(map[string]config.ProviderConfig, len(cfg.Providers))}
		for _, p := range cfg.Providers {
			if p.Name == "" {
				return nil, fmt.Errorf("provider with empty Name")
			}
			out.MCPServers[p.Name] = config.ProviderConfig{
				Command:     p.Command,
				Args:        p.Args,
				Env:         p.Env,
				Description: p.Description,
			}
		}
		return out, nil
	}

	path, err := config.ResolvePath(cfg.ConfigPath)
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}
