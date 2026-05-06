package engine

import (
	"reflect"
	"testing"
)

func TestParse_TopLevelFields(t *testing.T) {
	q, err := Parse(`{ viewer { login } pull_requests(state: "open") { title number } }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := len(q.TopLevel), 2; got != want {
		t.Fatalf("top-level: got %d, want %d", got, want)
	}
	if q.TopLevel[0].Name != "viewer" {
		t.Errorf("first field: got %q, want viewer", q.TopLevel[0].Name)
	}
	if q.TopLevel[1].Name != "pull_requests" {
		t.Errorf("second field: got %q, want pull_requests", q.TopLevel[1].Name)
	}
	if got, want := len(q.TopLevel[1].Args), 1; got != want {
		t.Fatalf("pull_requests args: got %d, want %d", got, want)
	}
	if q.TopLevel[1].Args[0].Name != "state" {
		t.Errorf("arg name: got %q, want state", q.TopLevel[1].Args[0].Name)
	}
	if q.TopLevel[1].Args[0].Value != "open" {
		t.Errorf("arg value: got %v, want open", q.TopLevel[1].Args[0].Value)
	}
}

func TestParse_Aliases(t *testing.T) {
	q, err := Parse(`{ openPRs: pull_requests(state: "open") { title } }`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	n := q.TopLevel[0]
	if n.Alias != "openPRs" {
		t.Errorf("alias: got %q, want openPRs", n.Alias)
	}
	if n.Name != "pull_requests" {
		t.Errorf("name: got %q, want pull_requests", n.Name)
	}
	if n.AliasOrName() != "openPRs" {
		t.Errorf("AliasOrName: got %q, want openPRs", n.AliasOrName())
	}
}

func TestShape_HashIgnoresAliasesAndArgValues(t *testing.T) {
	a, _ := Parse(`{ pull_requests(state: "open", since: "2026-01-01") { title number } }`)
	b, _ := Parse(`{ alias: pull_requests(since: "1999-01-01", state: "closed") { number title } }`)

	hashA := a.TopLevel[0].Shape().L1Hash()
	hashB := b.TopLevel[0].Shape().L1Hash()
	if hashA != hashB {
		t.Errorf("expected same hash regardless of alias / arg values / arg order / selection order:\nA = %s\nB = %s",
			hashA, hashB)
	}
}

func TestShape_HashDistinguishesShape(t *testing.T) {
	a, _ := Parse(`{ pull_requests(state: "x") { title number } }`)
	b, _ := Parse(`{ pull_requests(state: "x") { title number author { login } } }`)

	if a.TopLevel[0].Shape().L1Hash() == b.TopLevel[0].Shape().L1Hash() {
		t.Error("queries with different selection sets must produce different L1 hashes")
	}
}

func TestShape_NestedCanonicalisation(t *testing.T) {
	a, _ := Parse(`{ a { c b } }`)
	b, _ := Parse(`{ a { b c } }`)
	if !reflect.DeepEqual(a.TopLevel[0].Shape(), b.TopLevel[0].Shape()) {
		t.Errorf("Shape() should canonicalise child order:\nA = %+v\nB = %+v",
			a.TopLevel[0].Shape(), b.TopLevel[0].Shape())
	}
}

func TestStaticResolver_Register(t *testing.T) {
	r := NewStaticResolver()
	if err := r.Register(`{ viewer { login } }`, "def execute(args, parent): return {}"); err != nil {
		t.Fatalf("register: %v", err)
	}

	q, _ := Parse(`{ viewer { login } }`)
	src, ok := r.Resolve(q.TopLevel[0].Shape())
	if !ok {
		t.Fatal("expected resolver hit, got miss")
	}
	if src == "" {
		t.Error("script source was empty")
	}

	// A query whose shape doesn't match should miss.
	q2, _ := Parse(`{ viewer { name } }`)
	if _, ok := r.Resolve(q2.TopLevel[0].Shape()); ok {
		t.Error("resolver hit on non-matching shape")
	}
}
