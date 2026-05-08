// Package envfile loads a .env file into the process environment.
// Used only by the dev/CI eval entry point — the user-facing binary
// reads normal process env and never touches .env on its own.
//
// Tiny, deliberate replacement for github.com/joho/godotenv — we
// don't need quoting rules, multi-line values, or variable expansion.
//
// Existing process env always wins. Lines starting with '#' or blank
// are ignored. KEY=VALUE only.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Load reads ./.env if present and applies it to the process
// environment. A missing file is fine (returns nil). Existing env
// vars take precedence over file contents.
func Load() error {
	return LoadPath(".env")
}

// LoadPath reads the named env file and applies it to the process
// environment. Same semantics as Load: missing file is fine,
// existing env wins over file contents.
func LoadPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			return fmt.Errorf(".env:%d: malformed line (expected KEY=VALUE)", lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return fmt.Errorf(".env:%d: setenv %s: %w", lineNo, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	return nil
}
