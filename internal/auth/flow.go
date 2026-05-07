package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
)

// FlowConfig configures one interactive auth round.
type FlowConfig struct {
	// ProviderName is the routing key (used for logging + the
	// dynamic-client-registration `client_name`).
	ProviderName string

	// ServerURL is the MCP server's base URL. Used for OAuth
	// metadata discovery (well-known endpoints).
	ServerURL string

	// Scopes is the list of OAuth scopes to request.
	Scopes []string

	// ClientID, when non-empty, is used directly instead of running
	// dynamic client registration. Required for providers like
	// Slack, Google, etc. that don't implement RFC 7591.
	ClientID string

	// ClientSecret accompanies ClientID for confidential clients.
	ClientSecret string

	// Vault persists state across runs. The flow saves the token
	// (via the underlying mcp-go TokenStore) plus any client-
	// registration metadata it obtains here.
	Vault Vault

	// Port is the local port the callback listener binds to. If
	// 0, an ephemeral port is allocated; the redirect URI then
	// reflects the chosen port. Most OAuth servers accept any
	// loopback port per RFC 8252 §7.3.
	Port int

	// OpenBrowser opens a URL in the user's browser. Defaults to
	// the cross-platform OpenURL helper. Tests can override.
	OpenBrowser func(url string) error
}

// Run drives one OAuth authorization-code flow with PKCE, including
// dynamic client registration if needed. On success the resulting
// token is persisted via the Vault and is ready for the next MCP
// request through transport.OAuthHandler.
//
// This call BLOCKS until the user completes the flow in their
// browser (or ctx times out). Callers expecting non-interactive
// runs (e.g. CI) should set a short ctx deadline.
func Run(ctx context.Context, cfg FlowConfig) error {
	if cfg.OpenBrowser == nil {
		cfg.OpenBrowser = openURL
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("auth: bind callback listener: %w", err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://localhost:%d/callback", port)

	// Load or initialise the persisted state. RegisterClient mutates
	// the OAuthConfig in place when it succeeds, so we capture the
	// final values back into our State afterwards.
	state, err := cfg.Vault.Load(cfg.ProviderName)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("auth: load state: %w", err)
		}
		state = &State{}
	}
	state.RedirectURI = redirectURI
	state.Scopes = cfg.Scopes
	// Caller-provided credentials win over whatever was persisted
	// (e.g. user passed --client-id to override a stale one).
	if cfg.ClientID != "" {
		state.ClientID = cfg.ClientID
		state.ClientSecret = cfg.ClientSecret
	}
	if err := cfg.Vault.Save(cfg.ProviderName, state); err != nil {
		return fmt.Errorf("auth: save initial state: %w", err)
	}

	tokenStore := NewTokenStore(cfg.Vault, cfg.ProviderName)
	handler := transport.NewOAuthHandler(transport.OAuthConfig{
		ClientID:     state.ClientID,
		ClientSecret: state.ClientSecret,
		RedirectURI:  redirectURI,
		Scopes:       cfg.Scopes,
		TokenStore:   tokenStore,
		PKCEEnabled:  true,
	})
	handler.SetBaseURL(cfg.ServerURL)

	// Dynamic client registration if we don't have credentials yet.
	// If the server doesn't implement RFC 7591 (Slack, Google,
	// GitHub OAuth apps, etc.) the user must register a client
	// manually with the provider and pass the resulting credentials
	// via --client-id / --client-secret.
	if state.ClientID == "" {
		if err := handler.RegisterClient(ctx, "Genie ("+cfg.ProviderName+")"); err != nil {
			return fmt.Errorf("auth: dynamic client registration not supported by this server (%s).\n"+
				"Register a client manually at the provider's app settings and re-run with:\n"+
				"  genie auth %s --client-id <id> [--client-secret <secret>]",
				summariseDCRError(err), cfg.ProviderName)
		}
		state.ClientID = handler.GetClientID()
		state.ClientSecret = handler.GetClientSecret()
		if err := cfg.Vault.Save(cfg.ProviderName, state); err != nil {
			return fmt.Errorf("auth: save client registration: %w", err)
		}
	}

	verifier, err := transport.GenerateCodeVerifier()
	if err != nil {
		return fmt.Errorf("auth: generate code verifier: %w", err)
	}
	challenge := transport.GenerateCodeChallenge(verifier)
	stateParam, err := transport.GenerateState()
	if err != nil {
		return fmt.Errorf("auth: generate state: %w", err)
	}

	authURL, err := handler.GetAuthorizationURL(ctx, stateParam, challenge)
	if err != nil {
		return fmt.Errorf("auth: build authorization URL: %w", err)
	}

	// Spin up the callback listener before opening the browser so
	// we never miss the redirect.
	type callbackResult struct {
		code  string
		state string
		err   error
	}
	results := make(chan callbackResult, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if errCode := q.Get("error"); errCode != "" {
			msg := errCode
			if d := q.Get("error_description"); d != "" {
				msg += ": " + d
			}
			writeCallbackHTML(w, false, "Authorization failed: "+msg)
			results <- callbackResult{err: fmt.Errorf("auth server returned: %s", msg)}
			return
		}
		code := q.Get("code")
		st := q.Get("state")
		if code == "" {
			writeCallbackHTML(w, false, "Authorization callback missing `code`.")
			results <- callbackResult{err: errors.New("callback missing code")}
			return
		}
		writeCallbackHTML(w, true, "You can close this tab and return to Genie.")
		results <- callbackResult{code: code, state: st}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	slog.Info("auth: opening browser",
		"provider", cfg.ProviderName,
		"redirect_uri", redirectURI,
		"backend", cfg.Vault.Backend())
	_, _ = fmt.Fprintf(stderrWriter(), "Opening browser to authorize Genie for %q…\n", cfg.ProviderName)
	_, _ = fmt.Fprintf(stderrWriter(), "  If your browser doesn't open, visit:\n  %s\n", authURL)
	if err := cfg.OpenBrowser(authURL); err != nil {
		slog.Warn("auth: could not open browser", "err", err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-results:
		if res.err != nil {
			return res.err
		}
		if err := handler.ProcessAuthorizationResponse(ctx, res.code, res.state, verifier); err != nil {
			return fmt.Errorf("auth: exchange code for token: %w", err)
		}
	}

	slog.Info("auth: token obtained", "provider", cfg.ProviderName)
	return nil
}

// summariseDCRError reduces a long upstream error (often an HTML
// 404 page from a non-MCP-aware host) to something readable. Pulls
// the status code if present, drops everything after the first
// 120 chars of the body.
func summariseDCRError(err error) string {
	msg := err.Error()
	// Common shape from mcp-go: "registration request failed with
	// status NNN: <body>". Truncate body.
	for _, marker := range []string{"with status ", "status="} {
		if i := indexOf(msg, marker); i >= 0 {
			rest := msg[i+len(marker):]
			head := truncate(rest, 80)
			return marker + head
		}
	}
	return truncate(msg, 120)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// openURL opens the URL in the user's default browser.
func openURL(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	return exec.Command(cmd, args...).Start()
}

func writeCallbackHTML(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	status := "Success"
	if !ok {
		status = "Error"
	}
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html><head><title>Genie — %s</title></head>
<body style="font-family: system-ui; max-width: 32rem; margin: 4rem auto; padding: 0 1rem;">
<h1>Genie — %s</h1>
<p>%s</p>
</body></html>`, status, status, msg)
}
