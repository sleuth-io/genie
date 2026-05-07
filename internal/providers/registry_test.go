package providers

import (
	"testing"

	"github.com/sleuth-io/genie/internal/config"
)

func TestDiffEntries_AddedRemovedReplaced(t *testing.T) {
	old := map[string]config.ProviderConfig{
		"keep":     {Command: "/bin/true"},
		"drop":     {Command: "/bin/false"},
		"mutated":  {URL: "https://old.example/mcp"},
		"sameURL":  {URL: "https://x.example/mcp", Description: "x"},
		"argsDiff": {Command: "/bin/cmd", Args: []string{"a", "b"}},
	}
	next := map[string]config.ProviderConfig{
		"keep":     {Command: "/bin/true"},
		"new":      {URL: "https://new.example/mcp"},
		"mutated":  {URL: "https://new.example/mcp"},
		"sameURL":  {URL: "https://x.example/mcp", Description: "x"},
		"argsDiff": {Command: "/bin/cmd", Args: []string{"a", "b", "c"}},
	}

	added, removed, replaced := diffEntries(old, next)

	if !hasName(added, "new") || len(added) != 1 {
		t.Errorf("added = %v, want only [new]", names(added))
	}
	if !hasName(removed, "drop") || len(removed) != 1 {
		t.Errorf("removed = %v, want only [drop]", names(removed))
	}
	if !hasName(replaced, "mutated") || !hasName(replaced, "argsDiff") || len(replaced) != 2 {
		t.Errorf("replaced = %v, want [mutated argsDiff]", names(replaced))
	}
}

func TestSameEntry_DetectsAllFields(t *testing.T) {
	base := config.ProviderConfig{
		Command:     "/bin/c",
		Args:        []string{"a", "b"},
		Env:         map[string]string{"K": "V"},
		URL:         "",
		Type:        "stdio",
		Scopes:      []string{"read"},
		Headers:     map[string]string{"H": "v"},
		Description: "x",
	}
	if !sameEntry(base, base) {
		t.Fatal("identical entries should be equal")
	}
	mutations := []struct {
		name   string
		mutate func(p *config.ProviderConfig)
	}{
		{"command", func(p *config.ProviderConfig) { p.Command = "/bin/d" }},
		{"args", func(p *config.ProviderConfig) { p.Args = []string{"a"} }},
		{"env", func(p *config.ProviderConfig) { p.Env = map[string]string{"K": "W"} }},
		{"url", func(p *config.ProviderConfig) { p.URL = "https://x" }},
		{"type", func(p *config.ProviderConfig) { p.Type = "http" }},
		{"scopes", func(p *config.ProviderConfig) { p.Scopes = []string{"write"} }},
		{"headers", func(p *config.ProviderConfig) { p.Headers = map[string]string{} }},
		{"description", func(p *config.ProviderConfig) { p.Description = "y" }},
	}
	for _, m := range mutations {
		copy := base
		m.mutate(&copy)
		if sameEntry(base, copy) {
			t.Errorf("mutating %s should produce a different entry", m.name)
		}
	}
}

func TestEqualHelpers(t *testing.T) {
	// nil and empty slices represent the same "no args / no scopes"
	// state for our purposes; len-based comparison handles that.
	if !equalStringSlice(nil, []string{}) {
		t.Error("nil and empty slice should be equal")
	}
	if !equalStringSlice([]string{"a", "b"}, []string{"a", "b"}) {
		t.Error("equal slices reported unequal")
	}
	if equalStringSlice([]string{"a"}, []string{"a", "b"}) {
		t.Error("len-different slices reported equal")
	}
	if !equalStringMap(map[string]string{"k": "v"}, map[string]string{"k": "v"}) {
		t.Error("equal maps reported unequal")
	}
	if equalStringMap(map[string]string{"k": "v"}, map[string]string{"k": "w"}) {
		t.Error("differing values reported equal")
	}
	if equalStringMap(map[string]string{"a": "1"}, map[string]string{"b": "1"}) {
		t.Error("differing keys reported equal")
	}
}

func names(m map[string]config.ProviderConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hasName(m map[string]config.ProviderConfig, name string) bool {
	_, ok := m[name]
	return ok
}
