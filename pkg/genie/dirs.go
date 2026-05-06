package genie

import (
	"fmt"
	"os"
	"path/filepath"
)

// CacheDirEnvVar names the env var that overrides the default cache dir.
const CacheDirEnvVar = "GENIE_CACHE_DIR"

// resolveCacheDir applies the precedence: explicit override > env var >
// os.UserCacheDir()/genie/crystallized.
func resolveCacheDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if v := os.Getenv(CacheDirEnvVar); v != "" {
		return v, nil
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache dir: %w", err)
	}
	return filepath.Join(dir, "genie", "crystallized"), nil
}
