package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mrdon/gqlspike/internal/crystallize"
	"github.com/mrdon/gqlspike/internal/engine"
	"github.com/mrdon/gqlspike/internal/mcpclient"
	"github.com/mrdon/gqlspike/internal/plan"
	"github.com/mrdon/gqlspike/internal/runtime"
	"github.com/mrdon/gqlspike/internal/sandbox"
)

// engineBundle carries the live runtime resources for a query (or many
// queries during an eval). Closer tears them down in reverse order; safe to
// call after partial init failure (each Close is nil-checked).
type engineBundle struct {
	mcp       *mcpclient.Client
	monty     *runtime.MontyEngine
	store     *crystallize.Store
	generator *plan.Generator
	executor  *engine.Executor
}

// setupEngine boots the full pipeline: MCP client, monty runtime, host-
// function bridge, crystallize store, plan generator, executor. Used by
// both the one-shot `query` subcommand and the `eval` harness.
//
// crystallizedDir lets eval choose a clean directory per run if it wants
// cold-cache numbers; pass "./crystallized" for the default warm cache.
func setupEngine(ctx context.Context, crystallizedDir string) (*engineBundle, error) {
	mc, err := mcpclient.OpenGitHub(ctx)
	if err != nil {
		return nil, err
	}

	mEng, err := runtime.NewMontyEngineOwned()
	if err != nil {
		_ = mc.Close()
		return nil, fmt.Errorf("init monty engine: %w", err)
	}

	mcpFuncs, mcpParams := mcpclient.BuildHostFunctions(mc)
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

	store := crystallize.NewStore(crystallizedDir)
	gen := plan.NewGenerator(mc, store)

	ex := engine.NewExecutor(mEng, caps, store).WithGenerator(gen)
	return &engineBundle{
		mcp:       mc,
		monty:     mEng,
		store:     store,
		generator: gen,
		executor:  ex,
	}, nil
}

func (b *engineBundle) Close() {
	if b == nil {
		return
	}
	if b.monty != nil {
		_ = b.monty.Close()
	}
	if b.mcp != nil {
		_ = b.mcp.Close()
	}
}
