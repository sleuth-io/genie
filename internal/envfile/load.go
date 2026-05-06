// Package envfile auto-loads a .env file into the process environment at
// startup. Tiny, deliberate replacement for github.com/joho/godotenv — we
// don't need quoting rules, multi-line values, or variable expansion.
//
// Existing process env always wins. Lines starting with '#' or blank are
// ignored. KEY=VALUE only.
package envfile

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// PathEnvVar lets callers point the loader at an alternate file (e.g.
// during local testing, when secrets live in another project's .env).
// When set, the named file MUST exist; missing-file is treated as an
// error so that a typo in the path doesn't silently fall back to a
// wrong-but-present default.
const PathEnvVar = "GENIE_ENV_FILE"

// Load resolves which file to read and applies it. Behaviour:
//
//   - If GENIE_ENV_FILE is set: read that path. Missing file is an error.
//   - Otherwise: read ./.env if present. Missing file is fine.
//
// In either mode, existing env vars take precedence over file contents.
func Load() error {
	if path := os.Getenv(PathEnvVar); path != "" {
		return loadFile(path, false)
	}
	return loadFile(".env", true)
}

// loadFile reads `path` line-by-line. If `optional` is true, a missing
// file is silently skipped; otherwise it is reported as an error.
func loadFile(path string, optional bool) error {
	f, err := os.Open(path)
	if err != nil {
		if optional && os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open env file %q: %w", path, err)
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
			return fmt.Errorf("%s:%d: malformed line (expected KEY=VALUE)", path, lineNo)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if _, set := os.LookupEnv(key); set {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return fmt.Errorf("%s:%d: setenv %s: %w", path, lineNo, key, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %q: %w", path, err)
	}
	return nil
}
