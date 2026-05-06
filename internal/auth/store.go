package auth

import (
	"context"
	"errors"
	"sync"

	"github.com/mark3labs/mcp-go/client/transport"
)

// vaultStore adapts a Vault to mcp-go's transport.TokenStore for one
// provider. Reads/writes go through the Vault, preserving the rest
// of the per-provider State (client registration metadata) across
// token refreshes.
type vaultStore struct {
	mu       sync.Mutex
	vault    Vault
	provider string
}

// NewTokenStore returns a transport.TokenStore that persists tokens
// via the given Vault under the given provider name.
func NewTokenStore(vault Vault, provider string) transport.TokenStore {
	return &vaultStore{vault: vault, provider: provider}
}

func (v *vaultStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	state, err := v.vault.Load(v.provider)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, transport.ErrNoToken
		}
		return nil, err
	}
	if state.Token == nil {
		return nil, transport.ErrNoToken
	}
	return state.Token, nil
}

func (v *vaultStore) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	state, err := v.vault.Load(v.provider)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		state = &State{}
	}
	state.Token = token
	return v.vault.Save(v.provider, state)
}
