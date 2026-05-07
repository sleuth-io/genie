package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSession_AppendsJSONL(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvDir, dir)

	s := New()
	if s.Path() == "" {
		t.Fatal("session path should be set with a writable dir")
	}
	if !strings.HasPrefix(s.Path(), dir) {
		t.Errorf("session path %q not under temp dir %q", s.Path(), dir)
	}

	s.Append(Record{Call: "normalize", Provider: "github", UserText: "hello"})
	s.Append(Record{Call: "generate", Provider: "linear", UserText: "world", Err: "boom"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var lines []Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("decode line %q: %v", sc.Text(), err)
		}
		lines = append(lines, r)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d records, want 2", len(lines))
	}
	if lines[0].Call != "normalize" || lines[0].Provider != "github" || lines[0].SessionID != s.ID() {
		t.Errorf("first record = %+v", lines[0])
	}
	if lines[1].Err != "boom" {
		t.Errorf("second record err = %q, want boom", lines[1].Err)
	}
	if lines[0].Timestamp.IsZero() || lines[1].Timestamp.IsZero() {
		t.Error("timestamps should be auto-populated")
	}
}

func TestSession_NoOpOnUnwritableDir(t *testing.T) {
	// Force a path under /proc which can't be created.
	t.Setenv(EnvDir, "/proc/genie-cannot-write")
	s := New()
	if s.Path() != "" {
		t.Errorf("expected no-op session, got path %q", s.Path())
	}
	// Append must not panic.
	s.Append(Record{Call: "normalize"})
	if err := s.Close(); err != nil {
		t.Errorf("Close on no-op session: %v", err)
	}
}

func TestUserDataDir_Linux(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg-test")
	got, err := userDataDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/xdg-test" {
		t.Errorf("XDG_DATA_HOME should win: got %q", got)
	}
}

func TestUserDataDir_macOS(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/Users/test")
	prev := detectOS
	t.Cleanup(func() { detectOS = prev })
	detectOS = func() string { return "darwin" }
	got, err := userDataDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/Users/test", "Library", "Application Support")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
