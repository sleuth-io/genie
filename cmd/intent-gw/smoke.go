package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mrdon/gqlspike/internal/runtime"
)

// runSmoke exercises the embedded monty runtime end-to-end:
//   - boots wazero
//   - compiles monty.wasm
//   - runs a Python function with kwargs
//   - dispatches a host call back into Go
//   - prints the returned value
//
// If this passes, the wasm + wazero + host-call plumbing is alive and the
// rest of the spike can start using the runtime as a black box.
func runSmoke(ctx context.Context, _ []string) error {
	eng, err := runtime.NewMontyEngineOwned()
	if err != nil {
		return fmt.Errorf("init monty engine: %w", err)
	}
	defer eng.Close()

	src := `
def add(x, y):
    return {"sum": x + y, "label": label_of(x + y)}
`

	mod, err := eng.Compile(src)
	if err != nil {
		return fmt.Errorf("compile: %w", err)
	}

	caps := &runtime.Capabilities{
		BuiltIns: map[string]runtime.GoFunc{
			"label_of": func(_ context.Context, call *runtime.FunctionCall) (any, error) {
				v, _ := call.Args["n"].(float64)
				return fmt.Sprintf("the answer is %.0f", v), nil
			},
		},
		BuiltInParams: map[string][]string{
			"label_of": {"n"},
		},
		Limits: runtime.Limits{
			MaxDuration: 5 * time.Second,
		},
	}

	result, meta, err := eng.Run(ctx, mod, "add",
		map[string]any{"x": 19, "y": 23}, caps)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	fmt.Printf("result:        %v\n", result)
	fmt.Printf("duration_ms:   %d\n", meta.DurationMs)
	fmt.Printf("ext_calls:     %d\n", meta.ExternalCalls)
	return nil
}
