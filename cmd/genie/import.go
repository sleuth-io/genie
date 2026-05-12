package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sleuth-io/genie/internal/auth"
	"github.com/sleuth-io/genie/internal/claudecode"
	"github.com/sleuth-io/genie/internal/config"
)

// runMCPImport implements `genie mcp import`: scan Claude Code's MCP
// config (~/.claude.json + ./.mcp.json), let the user multi-select
// which entries to copy into genie's config, optionally run OAuth
// per HTTP provider, optionally remove the user-scope entry from
// Claude Code so Claude Code routes that provider through genie
// instead of calling it directly, and (if genie itself isn't already
// registered with Claude Code) offer to register it in
// ~/.claude.json user scope.
func runMCPImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp import", flag.ContinueOnError)
	setUsage(fs, "Usage: genie mcp import [--all] [--dry-run] [--config PATH]")
	configPath := fs.String("config", "", "override genie config path")
	all := fs.Bool("all", false, "import every entry without prompting")
	dryRun := fs.Bool("dry-run", false, "print what would be imported; don't write")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}
	scanner, err := claudecode.DefaultScanner(cwd)
	if err != nil {
		return err
	}
	entries, err := scanner.Scan()
	if err != nil {
		return err
	}

	path, err := config.ResolvePath(*configPath)
	if err != nil {
		return err
	}
	cfg, err := config.LoadForEdit(path)
	if err != nil {
		return err
	}

	var (
		candidates []claudecode.Entry
		skipped    []string
	)
	for _, e := range entries {
		// Skip genie itself — importing genie as a genie provider is
		// nonsensical (it would front itself).
		if e.Server.Command != "" && filepath.Base(e.Server.Command) == "genie" {
			continue
		}
		if _, exists := cfg.MCPServers[e.Name]; exists {
			skipped = append(skipped, e.Name)
		} else {
			candidates = append(candidates, e)
		}
	}

	if len(candidates) == 0 {
		fmt.Fprintln(os.Stderr, "No new MCP servers to import.")
		if len(skipped) > 0 {
			fmt.Fprintf(os.Stderr, "Already in genie config: %s\n", strings.Join(skipped, ", "))
		}
		return maybeRegisterGenie(scanner, *dryRun)
	}

	printCandidates(candidates, skipped)

	var picks []int
	if *all {
		picks = allIndices(len(candidates))
	} else {
		picks, err = promptMultiSelect(len(candidates))
		if err != nil {
			return err
		}
	}

	if len(picks) == 0 {
		fmt.Fprintln(os.Stderr, "Nothing selected.")
		return maybeRegisterGenie(scanner, *dryRun)
	}

	if *dryRun {
		fmt.Fprintln(os.Stderr, "\nWould import:")
		for _, i := range picks {
			fmt.Fprintf(os.Stderr, "  - %s [%s]\n", candidates[i].Name, candidates[i].Source)
		}
		return maybeRegisterGenie(scanner, *dryRun)
	}

	type pendingDelete struct {
		Name string
		Loc  claudecode.Locations
	}
	imported := 0
	var pendingDeletes []pendingDelete
	for _, i := range picks {
		e := candidates[i]
		prov := mapEntry(e)
		if err := validateProvider(e.Name, prov); err != nil {
			fmt.Fprintf(os.Stderr, "  ! %s: %v (skipped)\n", e.Name, err)
			continue
		}

		if prov.IsHTTP() && len(prov.Headers) == 0 {
			yes, err := promptYesNo(fmt.Sprintf("Authenticate %q now?", e.Name), false)
			if err != nil {
				return err
			}
			if yes {
				fmt.Fprintf(os.Stderr, "Authorizing %q…\n", e.Name)
				if err := auth.Run(ctx, auth.FlowConfig{
					ProviderName: e.Name,
					ServerURL:    prov.URL,
					Scopes:       prov.Scopes,
					Vault:        auth.Open(),
				}); err != nil {
					fmt.Fprintf(os.Stderr, "  ! auth failed for %s: %v (entry not imported)\n", e.Name, err)
					continue
				}
			}
		}

		cfg.MCPServers[e.Name] = prov
		imported++
		fmt.Fprintf(os.Stderr, "  ✓ %s [%s]\n", e.Name, e.Source)

		// Find every Claude Code scope where this name lives. The
		// entry we selected has its own Source, but the same name may
		// also be present in lower-precedence scopes (e.g. selected
		// from project-file but also in user-scope). Offering deletion
		// across all of them is what "I'm migrating to genie" means.
		// Defer the actual deletion until after genie's config Save
		// succeeds, so a save failure doesn't strand the user with no
		// entry on either side.
		loc, err := scanner.LocationsOf(e.Name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "    ! could not locate %s in Claude Code: %v\n", e.Name, err)
		} else if loc.Any() {
			yes, err := promptYesNo(
				fmt.Sprintf("    Remove %q from Claude Code (%s)?", e.Name, formatLocations(loc, scanner.ProjectMCPPath)),
				false,
			)
			if err != nil {
				return err
			}
			if yes {
				pendingDeletes = append(pendingDeletes, pendingDelete{Name: e.Name, Loc: loc})
			}
		}
	}

	if imported > 0 {
		if err := config.Save(path, cfg); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "\nImported %d provider(s) into %s.\n", imported, path)
	} else {
		fmt.Fprintln(os.Stderr, "\nNothing imported.")
	}

	for _, p := range pendingDeletes {
		if p.Loc.UserScope {
			if err := scanner.DeleteUserScopeMCPServer(p.Name); err != nil {
				fmt.Fprintf(os.Stderr, "  ! could not remove %s from user-scope: %v\n", p.Name, err)
			} else {
				fmt.Fprintf(os.Stderr, "  ↳ removed %s from user-scope (%s)\n", p.Name, scanner.ClaudeJSONPath)
			}
		}
		if p.Loc.ProjectLocal {
			if err := scanner.DeleteProjectLocalMCPServer(p.Name); err != nil {
				fmt.Fprintf(os.Stderr, "  ! could not remove %s from project-local: %v\n", p.Name, err)
			} else {
				fmt.Fprintf(os.Stderr, "  ↳ removed %s from project-local\n", p.Name)
			}
		}
		if p.Loc.ProjectFile {
			if err := scanner.DeleteProjectFileMCPServer(p.Name); err != nil {
				fmt.Fprintf(os.Stderr, "  ! could not remove %s from %s: %v\n", p.Name, scanner.ProjectMCPPath, err)
			} else {
				fmt.Fprintf(os.Stderr, "  ↳ removed %s from %s\n", p.Name, scanner.ProjectMCPPath)
			}
		}
	}

	return maybeRegisterGenie(scanner, false)
}

// formatLocations renders a Locations as a human-readable list for the
// removal prompt. Project-file is annotated as "committed" so the user
// knows the delete will modify a tracked file.
func formatLocations(loc claudecode.Locations, projectMCPPath string) string {
	var parts []string
	if loc.UserScope {
		parts = append(parts, "user-scope")
	}
	if loc.ProjectLocal {
		parts = append(parts, "project-local")
	}
	if loc.ProjectFile {
		parts = append(parts, fmt.Sprintf("%s — committed", projectMCPPath))
	}
	return strings.Join(parts, ", ")
}

func mapEntry(e claudecode.Entry) config.ProviderConfig {
	return config.ProviderConfig{
		Command:     e.Server.Command,
		Args:        e.Server.Args,
		Env:         e.Server.Env,
		URL:         e.Server.URL,
		Type:        e.Server.Type,
		Headers:     e.Server.Headers,
		Description: fmt.Sprintf("imported from Claude Code (%s)", e.Source),
	}
}

func maybeRegisterGenie(scanner *claudecode.Scanner, dryRun bool) error {
	has, err := scanner.HasGenieUserScope()
	if err != nil {
		return err
	}
	if has {
		fmt.Fprintf(os.Stderr, "\nGenie is already registered with Claude Code (user-scope).\n")
		return nil
	}
	yes, err := promptYesNo("\nRegister genie with Claude Code (user scope, available in every project)?", true)
	if err != nil {
		return err
	}
	if !yes {
		fmt.Fprintln(os.Stderr, "Skipped registering genie. Add it later with `claude mcp add --scope user genie -- <path>/genie serve`.")
		return nil
	}
	if dryRun {
		fmt.Fprintln(os.Stderr, "(dry-run) Would add genie entry to user-scope mcpServers in", scanner.ClaudeJSONPath)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate genie executable: %w", err)
	}
	if err := scanner.WriteUserScopeMCPServer("genie", claudecode.MCPServer{
		Command: exe,
		Args:    []string{"serve"},
	}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "✓ Registered genie in %s user scope (command=%s, args=[serve])\n", scanner.ClaudeJSONPath, exe)
	return nil
}

func printCandidates(candidates []claudecode.Entry, skipped []string) {
	fmt.Fprintln(os.Stderr, "Available MCP servers from Claude Code:")
	for i, e := range candidates {
		transport := "stdio"
		target := e.Server.Command
		if len(e.Server.Args) > 0 {
			target += " " + strings.Join(e.Server.Args, " ")
		}
		if e.Server.URL != "" {
			transport = "http"
			if e.Server.Type == "sse" {
				transport = "sse"
			}
			target = e.Server.URL
		}
		fmt.Fprintf(os.Stderr, "  %d. %-20s [%-14s] %-5s %s\n", i+1, e.Name, e.Source, transport, target)
	}
	if len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "\nAlready in genie config (skipped): %s\n", strings.Join(skipped, ", "))
	}
}

func allIndices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func promptMultiSelect(n int) ([]int, error) {
	fmt.Fprintf(os.Stderr, "\nSelect [e.g. 1,3 / all / none] (default: all): ")
	line, err := readLine()
	if err != nil {
		return nil, err
	}
	line = strings.TrimSpace(line)
	switch strings.ToLower(line) {
	case "", "all":
		return allIndices(n), nil
	case "none", "q", "quit":
		return nil, nil
	}
	parts := strings.Split(line, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx, err := strconv.Atoi(p)
		if err != nil || idx < 1 || idx > n {
			return nil, fmt.Errorf("invalid selection %q (want 1..%d, comma-separated)", p, n)
		}
		out = append(out, idx-1)
	}
	return out, nil
}

func promptYesNo(question string, defaultYes bool) (bool, error) {
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	fmt.Fprintf(os.Stderr, "%s %s: ", question, suffix)
	line, err := readLine()
	if err != nil {
		return false, err
	}
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes, nil
	}
	return line == "y" || line == "yes", nil
}

var stdinReader = bufio.NewReader(os.Stdin)

func readLine() (string, error) {
	s, err := stdinReader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return s, nil
}
