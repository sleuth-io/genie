package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mrdon/gqlspike/internal/runtime"
)

// runProbe smoke-tests which Python idioms work inside the embedded monty
// runtime. Used to write the LLM system prompt's "available stdlib" section
// — the LLM was producing scripts that called str.format() and datetime.now()
// which the sandbox doesn't support, dragging hypothesis 1 below threshold.
func runProbe(_ context.Context, _ []string) error {
	eng, err := runtime.NewMontyEngineOwned()
	if err != nil {
		return err
	}
	defer eng.Close()

	checks := []struct {
		name string
		code string
	}{
		{"str.format", `def go(): return "hello {}".format("world")`},
		{"f-string", `def go(): w="world"; return f"hello {w}"`},
		{"%-format", `def go(): return "hello %s" % "world"`},
		{"json.dumps", `import json
def go(): return json.dumps({"a":1})`},
		{"json.loads", `import json
def go(): return json.loads('{"a":1}')`},
		{"re.search", `import re
def go():
    m = re.search(r"\d+", "abc123")
    return m.group() if m else None`},
		{"datetime import", `import datetime
def go(): return "ok"`},
		{"datetime.datetime.now", `import datetime
def go(): return str(datetime.datetime.now())`},
		{"datetime.datetime", `import datetime
def go(): return str(datetime.datetime(2024,1,1))`},
		{"datetime.timedelta", `import datetime
def go(): return str(datetime.timedelta(days=30))`},
		{"datetime.fromisoformat", `import datetime
def go(): return str(datetime.datetime.fromisoformat("2024-01-01T00:00:00"))`},
		{"sorted", `def go(): return sorted([3,1,2])`},
		{"list.sort", `def go():
    a = [3,1,2]
    a.sort()
    return a`},
		{"comprehension", `def go(): return [x*2 for x in range(3)]`},
		{"dict comprehension", `def go(): return {x: x*2 for x in range(3)}`},
		{"isinstance", `def go(): return isinstance("x", str)`},
	}

	caps := &runtime.Capabilities{
		Limits: runtime.Limits{MaxDuration: 5 * time.Second},
	}

	for _, c := range checks {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		mod, _ := eng.Compile(c.code)
		v, _, err := eng.Run(ctx, mod, "go", nil, caps)
		cancel()
		status := "OK"
		extra := fmt.Sprintf(" -> %v", v)
		if err != nil {
			status = "FAIL"
			extra = " (" + err.Error() + ")"
		}
		fmt.Printf("%-26s %s%s\n", c.name, status, extra)
	}
	return nil
}
