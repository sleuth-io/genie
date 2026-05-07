// Package auth handles OAuth credentials for HTTP-transport MCP
// providers. It owns three things:
//
//   - persistence of access + refresh tokens (Vault interface, with
//     a keyring-backed implementation and a file-backed fallback);
//   - persistence of dynamic-client-registration metadata (client_id,
//     redirect_uri) alongside the token, so we don't re-register on
//     every restart;
//   - the interactive flow that obtains a fresh token (opens the
//     browser, listens on a local callback URL, exchanges the code).
//
// Use Open() to get a Vault, then construct a Store from it to plug
// into mcp-go's transport.OAuthConfig.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/zalando/go-keyring"
)

// keyringService is the namespace used in OS keychain entries.
const keyringService = "genie"

// State is what we persist per provider: client registration plus the
// current OAuth token. A nil Token means "registered but not yet
// authorized" (or the user explicitly logged out).
type State struct {
	ClientID     string           `json:"client_id,omitempty"`
	ClientSecret string           `json:"client_secret,omitempty"`
	RedirectURI  string           `json:"redirect_uri,omitempty"`
	Scopes       []string         `json:"scopes,omitempty"`
	Token        *transport.Token `json:"token,omitempty"`
}

// Vault stores per-provider OAuth state. Implementations must be
// safe to call concurrently for distinct providers.
type Vault interface {
	Load(provider string) (*State, error)
	Save(provider string, state *State) error
	Delete(provider string) error
	Backend() string
}

// ErrNotFound is returned by Vault.Load when no state exists.
var ErrNotFound = errors.New("auth: no state for provider")

// Open returns a Vault using the OS keychain when available, falling
// back to file-backed storage under $GENIE_CONFIG_DIR/auth/. The
// keyring backend is preferred because it leverages the system's
// secure-credential storage (Keychain on macOS, Secret Service on
// Linux, Credential Manager on Windows) rather than putting tokens
// on disk in plain JSON.
//
// GENIE_AUTH_BACKEND=keyring|file pins one explicitly.
func Open() Vault {
	switch os.Getenv("GENIE_AUTH_BACKEND") {
	case "file":
		return openFileVault()
	case "keyring":
		return &keyringVault{}
	}
	if probeKeyring() {
		return &keyringVault{}
	}
	slog.Info("auth: keyring unavailable, using file storage")
	return openFileVault()
}

// probeKeyring attempts a no-op keyring write/delete to check the
// platform actually has a working credential store. Headless Linux
// boxes without a Secret Service usually fail here.
func probeKeyring() bool {
	const probe = "__probe__"
	if err := keyring.Set(keyringService, probe, "x"); err != nil {
		return false
	}
	_ = keyring.Delete(keyringService, probe)
	return true
}

// keyringVault stores the JSON-encoded State in the OS keychain.
type keyringVault struct{}

func (k *keyringVault) Backend() string { return "keyring" }

func (k *keyringVault) Load(provider string) (*State, error) {
	raw, err := keyring.Get(keyringService, provider)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("keyring get %q: %w", provider, err)
	}
	var s State
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("keyring decode %q: %w", provider, err)
	}
	return &s, nil
}

func (k *keyringVault) Save(provider string, state *State) error {
	buf, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := keyring.Set(keyringService, provider, string(buf)); err != nil {
		return fmt.Errorf("keyring set %q: %w", provider, err)
	}
	return nil
}

func (k *keyringVault) Delete(provider string) error {
	if err := keyring.Delete(keyringService, provider); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("keyring delete %q: %w", provider, err)
	}
	return nil
}

// fileVault stores State as JSON files under $GENIE_CONFIG_DIR/auth/.
type fileVault struct {
	root string
}

func openFileVault() *fileVault {
	dir, err := os.UserConfigDir()
	if err != nil {
		// Last-resort fallback: CWD. UserConfigDir failures are
		// extremely rare; better to keep working than to crash.
		dir = "."
	}
	return &fileVault{root: filepath.Join(dir, "genie", "auth")}
}

func (f *fileVault) Backend() string { return "file" }

func (f *fileVault) path(provider string) string {
	return filepath.Join(f.root, provider+".json")
}

func (f *fileVault) Load(provider string) (*State, error) {
	raw, err := os.ReadFile(f.path(provider))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("file vault read %q: %w", provider, err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("file vault decode %q: %w", provider, err)
	}
	return &s, nil
}

func (f *fileVault) Save(provider string, state *State) error {
	if err := os.MkdirAll(f.root, 0o700); err != nil {
		return fmt.Errorf("file vault mkdir: %w", err)
	}
	buf, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	tmp := f.path(provider) + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return fmt.Errorf("file vault write %q: %w", provider, err)
	}
	if err := os.Rename(tmp, f.path(provider)); err != nil {
		return fmt.Errorf("file vault rename %q: %w", provider, err)
	}
	return nil
}

func (f *fileVault) Delete(provider string) error {
	if err := os.Remove(f.path(provider)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("file vault delete %q: %w", provider, err)
	}
	return nil
}
