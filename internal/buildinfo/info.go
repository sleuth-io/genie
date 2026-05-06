// Package buildinfo holds variables populated at build time via -ldflags.
// They surface in the MCP server-info handshake and the --version output.
package buildinfo

var (
	// Version is set via -ldflags during build (defaults to "dev"
	// for `go build` / `go install` invocations without the flag).
	Version = "dev"
	// Commit is the short git SHA, set via -ldflags.
	Commit = "none"
	// Date is the build timestamp in RFC3339 form, set via -ldflags.
	Date = "unknown"
)

// GetUserAgent returns a User-Agent string suitable for outbound HTTP
// requests Genie makes (e.g. to the Anthropic API).
func GetUserAgent() string {
	return "genie/" + Version
}
