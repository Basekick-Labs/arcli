// Shell completion wiring. Cobra generates the `arcli completion
// bash|zsh|fish|powershell` scripts; this file teaches them what arcli
// values look like so <TAB> offers connection names and enum values
// instead of the current directory's files.
package commands

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/basekick-labs/arcli/internal/config"
)

// installCompletions walks the finished command tree once and registers
// completion functions by flag name, so the 40-odd `--output` flags do
// not each need a call site:
//
//   - --connection / -c: names from the config file (best effort).
//   - --file / -f, and any flag declared with MarkFlagFilename
//     (cq --query-file): file completion.
//   - any flag whose usage lists values as `a|b|c`: those values.
//   - everything else: no file completion. Completing `--database <TAB>`
//     with the cwd listing is worse than completing nothing.
//
// Positional <name> on `config update|set-active|delete` completes to
// connection names as well.
func installCompletions(root *cobra.Command) {
	root.CompletionOptions.SetDefaultShellCompDirective(cobra.ShellCompDirectiveNoFileComp)
	walkCommands(root, func(c *cobra.Command) {
		c.Flags().VisitAll(func(f *pflag.Flag) { registerFlagCompletion(c, f) })
	})
	for _, name := range []string{"update", "set-active", "delete"} {
		if c, _, err := root.Find([]string{"config", name}); err == nil && c.Name() == name {
			c.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				if len(args) > 0 {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}
				return completeConnectionNames(cmd, args, toComplete)
			}
		}
	}
}

func walkCommands(c *cobra.Command, fn func(*cobra.Command)) {
	fn(c)
	for _, sub := range c.Commands() {
		walkCommands(sub, fn)
	}
}

func registerFlagCompletion(c *cobra.Command, f *pflag.Flag) {
	var fn cobra.CompletionFunc
	switch f.Name {
	case "connection":
		fn = completeConnectionNames
	case "file":
		fn = cobra.FixedCompletions(nil, cobra.ShellCompDirectiveDefault)
	default:
		if _, isPath := f.Annotations[cobra.BashCompFilenameExt]; isPath {
			// Declared with MarkFlagFilename (e.g. cq --query-file).
			fn = cobra.FixedCompletions(nil, cobra.ShellCompDirectiveDefault)
			break
		}
		vals := enumFromUsage(f.Usage)
		if len(vals) == 0 {
			return
		}
		fn = cobra.FixedCompletions(vals, cobra.ShellCompDirectiveNoFileComp)
	}
	// Registration fails only for a flag registered twice; every flag is
	// visited once per tree, so ignoring the error is safe.
	_ = c.RegisterFlagCompletionFunc(f.Name, fn)
}

// enumFromUsage extracts `a|b|c` from a usage string such as
// "output format: table|json|csv" or "permission to grant
// (read|write|delete|admin); repeat or comma-join". Only the first
// pipe-joined token counts; values are lower-case identifiers.
func enumFromUsage(usage string) []cobra.Completion {
	for _, tok := range strings.Fields(usage) {
		if !strings.Contains(tok, "|") {
			continue
		}
		tok = strings.Trim(tok, "();:,.")
		parts := strings.Split(tok, "|")
		out := make([]cobra.Completion, 0, len(parts))
		for _, p := range parts {
			if p == "" || strings.Trim(p, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
				return nil
			}
			out = append(out, cobra.Completion(p))
		}
		return out
	}
	return nil
}

// completeConnectionNames lists the connections in the config file with
// the endpoint as the description and "(active)" on the active one.
// Anything that goes wrong (no file, unreadable, malformed) completes
// to nothing: the completion stream is not the place for an error.
func completeConnectionNames(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
	cfg, err := config.Load()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(cfg.Connections))
	for name := range cfg.Connections {
		if strings.HasPrefix(name, toComplete) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]cobra.Completion, 0, len(names))
	for _, name := range names {
		desc := cfg.Connections[name].Endpoint
		if name == cfg.Active {
			desc += " (active)"
		}
		out = append(out, cobra.CompletionWithDesc(name, desc))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
