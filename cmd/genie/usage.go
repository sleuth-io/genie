package main

import (
	"flag"
	"fmt"
	"strings"
)

// setUsage replaces a FlagSet's auto-generated `-h` output with a
// GNU-style version: long flags print as `--name` (the stdlib flag
// package prints `-name`, which reads as Go-internal-tool-style and
// trips up muscle memory built on every other CLI). Flags registered
// with a description starting "shorthand for --<long>" are folded into
// the same line as their long counterpart instead of getting a
// separate entry.
//
// Invocation behaviour is unchanged: stdlib `flag` already accepts
// both `-name` and `--name` for any registered flag, so this is
// purely a help-text fix.
func setUsage(fs *flag.FlagSet, summary string) {
	fs.Usage = func() {
		var b strings.Builder
		if summary != "" {
			b.WriteString(summary)
			b.WriteString("\n\n")
		}
		b.WriteString("Flags:\n")
		writeFlagsWithDoubleDash(&b, fs)
		_, _ = fs.Output().Write([]byte(b.String()))
	}
}

func writeFlagsWithDoubleDash(b *strings.Builder, fs *flag.FlagSet) {
	shorts := map[string][]string{}
	fs.VisitAll(func(f *flag.Flag) {
		if target := shorthandTarget(f.Usage); target != "" {
			shorts[target] = append(shorts[target], f.Name)
		}
	})

	fs.VisitAll(func(f *flag.Flag) {
		if shorthandTarget(f.Usage) != "" {
			return
		}
		var names []string
		for _, s := range shorts[f.Name] {
			names = append(names, "-"+s)
		}
		names = append(names, longOrShort(f.Name))
		fmt.Fprintf(b, "  %s", strings.Join(names, ", "))
		if typeHint, _ := flag.UnquoteUsage(f); typeHint != "" {
			fmt.Fprintf(b, " %s", typeHint)
		}
		b.WriteString("\n")
		fmt.Fprintf(b, "        %s", f.Usage)
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
			fmt.Fprintf(b, " (default %q)", f.DefValue)
		}
		b.WriteString("\n")
	})
}

func longOrShort(name string) string {
	if len(name) > 1 {
		return "--" + name
	}
	return "-" + name
}

func shorthandTarget(usage string) string {
	const prefix = "shorthand for --"
	if strings.HasPrefix(usage, prefix) {
		return strings.TrimPrefix(usage, prefix)
	}
	return ""
}
