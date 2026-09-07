// scheduler subcommand: show the state of Arc's background schedulers
// (continuous queries and retention). On Arc OSS neither scheduler runs
// (Enterprise feature); this command is the quickest way to explain why
// a new CQ or policy never fires.
package commands

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newSchedulerCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "scheduler",
		Short: "Inspect the continuous-query and retention schedulers",
	}
	c.AddCommand(newSchedulerStatusCmd())
	return c
}

func newSchedulerStatusCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "status",
		Short: "Show whether the CQ and retention schedulers are running",
		Long: `Show whether the CQ and retention schedulers are running
(GET /api/v1/schedulers/). Works with any token.

Each scheduler is either running (with its jobs or next run) or not
started, in which case the server states why — on Arc OSS the reason is
"Enterprise license required" and policies / continuous queries must be
run manually with "arcli retention execute" and "arcli cq execute".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			st, err := cli.SchedulerStatus(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, st.Raw)
			}
			fmt.Fprintf(w, "continuous queries:  %s\n", describeScheduler(st.CQ, true))
			fmt.Fprintf(w, "retention:           %s\n", describeScheduler(st.Retention, false))
			if !st.CQ.NotRunning() && len(st.CQ.Jobs) > 0 {
				fmt.Fprintln(w)
				rows := make([][]string, 0, len(st.CQ.Jobs))
				for _, j := range st.CQ.Jobs {
					rows = append(rows, []string{strconv.FormatInt(j.CQID, 10), clean(j.CQName), clean(j.Interval)})
				}
				return output.Table(w, []string{"CQ ID", "NAME", "INTERVAL"}, rows)
			}
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// describeScheduler renders one block from its fields: the `enabled`
// pointer selects the not-started shape; inside the running shape,
// `running` is the actual state (a scheduler whose Start failed is
// present with running=false).
func describeScheduler(b client.SchedulerBlock, isCQ bool) string {
	if b.NotRunning() {
		return "not running (" + clean(b.Reason) + ")"
	}
	parts := []string{}
	if b.Running {
		parts = append(parts, "running")
	} else {
		parts = append(parts, "present but not running")
	}
	if !b.LicenseValid {
		parts = append(parts, "license invalid")
	}
	if isCQ {
		parts = append(parts, fmt.Sprintf("%d scheduled job(s)", b.JobCount))
	} else {
		if b.Schedule != "" {
			parts = append(parts, "schedule "+clean(b.Schedule))
		}
		if b.NextRun != "" {
			parts = append(parts, "next run "+fmtServerTime(b.NextRun, "-"))
		}
		if b.CanRun != nil {
			role := ""
			if b.GateRole != "" {
				role = " (" + clean(b.GateRole) + ")"
			}
			parts = append(parts, fmt.Sprintf("can run on this node: %t%s", *b.CanRun, role))
		}
	}
	return strings.Join(parts, ", ")
}
