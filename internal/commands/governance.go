// governance commands manage enterprise query-governance policies and usage.
package commands

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newGovernanceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "governance",
		Short: "Manage query governance policies and usage (Enterprise)",
		Long: `Manage query-governance policies and per-token usage.

The governance endpoints require an admin token and an Enterprise licence
with query governance enabled. A limit of 0 means unlimited.`,
	}
	c.AddCommand(newGovernancePolicyCmd(), newGovernanceUsageCmd())
	return c
}

func newGovernancePolicyCmd() *cobra.Command {
	c := &cobra.Command{Use: "policy", Short: "Manage per-token query limits"}
	c.AddCommand(newGovernancePolicyListCmd(), newGovernancePolicyShowCmd(), newGovernancePolicyCreateCmd(), newGovernancePolicyUpdateCmd(), newGovernancePolicyDeleteCmd())
	return c
}

func resolveGovernancePolicyRef(ctx context.Context, cli *client.Client, ref string) (*client.TokenInfo, *client.GovernancePolicy, error) {
	token, err := resolveTokenRef(ctx, cli, ref)
	if err != nil {
		return nil, nil, err
	}
	policy, err := cli.GetGovernancePolicy(ctx, token.ID)
	if err != nil {
		return nil, nil, err
	}
	if policy.TokenName == "" {
		policy.TokenName = token.Name
	}
	return token, policy, nil
}

func newGovernancePolicyListCmd() *cobra.Command {
	var f connFlags
	var outputFormat string
	var noHeader bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List query-governance policies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			policies, err := cli.ListGovernancePolicies(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), struct {
					Policies []client.GovernancePolicy `json:"policies"`
					Count    int                       `json:"count"`
				}{policies, len(policies)})
			}
			return renderGovernancePolicyList(cmd.OutOrStdout(), policies, noHeader, outputFormat == output.FormatCSV)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

func newGovernancePolicyShowCmd() *cobra.Command {
	var f connFlags
	var outputFormat string
	c := &cobra.Command{
		Use:   "show <token-id|name>",
		Short: "Show a token's query-governance policy",
		Args:  cobra.ExactArgs(1),
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
			_, policy, err := resolveGovernancePolicyRef(ctx, cli, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), policy)
			}
			writeGovernancePolicyInfo(cmd.OutOrStdout(), policy)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

type governanceLimitFlags struct {
	rateLimitPerMinute int
	rateLimitPerHour   int
	maxQueriesPerHour  int
	maxQueriesPerDay   int
	maxRowsPerQuery    int
	maxScanDurationSec int
}

func (lf *governanceLimitFlags) add(c *cobra.Command) {
	c.Flags().IntVar(&lf.rateLimitPerMinute, "rate-limit-per-minute", 0, "maximum queries per minute (0 = unlimited)")
	c.Flags().IntVar(&lf.rateLimitPerHour, "rate-limit-per-hour", 0, "maximum queries per hour (0 = unlimited)")
	c.Flags().IntVar(&lf.maxQueriesPerHour, "max-queries-per-hour", 0, "maximum queries per hour (0 = unlimited)")
	c.Flags().IntVar(&lf.maxQueriesPerDay, "max-queries-per-day", 0, "maximum queries per day (0 = unlimited)")
	c.Flags().IntVar(&lf.maxRowsPerQuery, "max-rows-per-query", 0, "maximum rows returned by one query (0 = unlimited)")
	c.Flags().IntVar(&lf.maxScanDurationSec, "max-scan-duration-sec", 0, "maximum scan duration in seconds (0 = unlimited)")
}

func (lf governanceLimitFlags) request(tokenID int64) client.GovernancePolicyRequest {
	return client.GovernancePolicyRequest{
		TokenID: tokenID, RateLimitPerMinute: lf.rateLimitPerMinute, RateLimitPerHour: lf.rateLimitPerHour,
		MaxQueriesPerHour: lf.maxQueriesPerHour, MaxQueriesPerDay: lf.maxQueriesPerDay,
		MaxRowsPerQuery: lf.maxRowsPerQuery, MaxScanDurationSec: lf.maxScanDurationSec,
	}
}

func (lf governanceLimitFlags) merge(policy client.GovernancePolicy, cmd *cobra.Command) client.GovernancePolicyRequest {
	req := client.GovernancePolicyRequest{
		TokenID: policy.TokenID, RateLimitPerMinute: policy.RateLimitPerMinute, RateLimitPerHour: policy.RateLimitPerHour,
		MaxQueriesPerHour: policy.MaxQueriesPerHour, MaxQueriesPerDay: policy.MaxQueriesPerDay,
		MaxRowsPerQuery: policy.MaxRowsPerQuery, MaxScanDurationSec: policy.MaxScanDurationSec,
	}
	flags := cmd.Flags()
	if flags.Changed("rate-limit-per-minute") {
		req.RateLimitPerMinute = lf.rateLimitPerMinute
	}
	if flags.Changed("rate-limit-per-hour") {
		req.RateLimitPerHour = lf.rateLimitPerHour
	}
	if flags.Changed("max-queries-per-hour") {
		req.MaxQueriesPerHour = lf.maxQueriesPerHour
	}
	if flags.Changed("max-queries-per-day") {
		req.MaxQueriesPerDay = lf.maxQueriesPerDay
	}
	if flags.Changed("max-rows-per-query") {
		req.MaxRowsPerQuery = lf.maxRowsPerQuery
	}
	if flags.Changed("max-scan-duration-sec") {
		req.MaxScanDurationSec = lf.maxScanDurationSec
	}
	return req
}

func governanceLimitFlagsChanged(cmd *cobra.Command) bool {
	for _, name := range []string{"rate-limit-per-minute", "rate-limit-per-hour", "max-queries-per-hour", "max-queries-per-day", "max-rows-per-query", "max-scan-duration-sec"} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func newGovernancePolicyCreateCmd() *cobra.Command {
	var f connFlags
	var outputFormat string
	var limits governanceLimitFlags
	c := &cobra.Command{
		Use:   "create <token-id|name>",
		Short: "Create a query-governance policy (admin)",
		Example: `  arcli governance policy create batch --rate-limit-per-minute 30 --max-queries-per-day 5000
  arcli governance policy create 12 --max-rows-per-query 10000 --max-scan-duration-sec 60`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			req := limits.request(1)
			if err := client.ValidateGovernancePolicyRequest(req); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			token, err := resolveTokenRef(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			req.TokenID = token.ID
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			policy, err := cli.CreateGovernancePolicy(ctx, req)
			if err != nil {
				return err
			}
			if policy.TokenName == "" {
				policy.TokenName = token.Name
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), policy)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created governance policy for token %q (id %d)\n", clean(token.Name), token.ID)
			writeGovernancePolicyInfo(cmd.OutOrStdout(), policy)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	limits.add(c)
	return c
}

func newGovernancePolicyUpdateCmd() *cobra.Command {
	var f connFlags
	var outputFormat string
	var limits governanceLimitFlags
	c := &cobra.Command{
		Use:   "update <token-id|name>",
		Short: "Update selected limits on a token's query-governance policy (admin)",
		Long: `Update one or more policy limits. Arc replaces the complete policy on PUT,
so arcli reads the current policy and merges only the flags you pass.`,
		Example: `  arcli governance policy update batch --rate-limit-per-minute 45
  arcli governance policy update 12 --max-rows-per-query 0`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if !governanceLimitFlagsChanged(cmd) {
				return fmt.Errorf("nothing to update (pass at least one governance limit flag)")
			}
			validation := limits.request(1)
			if err := client.ValidateGovernancePolicyRequest(validation); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			token, policy, err := resolveGovernancePolicyRef(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			req := limits.merge(*policy, cmd)
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			updated, err := cli.UpdateGovernancePolicy(ctx, token.ID, req)
			if err != nil {
				return err
			}
			if updated.TokenName == "" {
				updated.TokenName = token.Name
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), updated)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated governance policy for token %q (id %d)\n", clean(token.Name), token.ID)
			writeGovernancePolicyInfo(cmd.OutOrStdout(), updated)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	limits.add(c)
	return c
}

func newGovernancePolicyDeleteCmd() *cobra.Command {
	var f connFlags
	var yes bool
	c := &cobra.Command{
		Use:   "delete <token-id|name>",
		Short: "Delete a token's query-governance policy (admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			token, policy, err := resolveGovernancePolicyRef(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete governance policy for token %q (id %d)?", clean(token.Name), token.ID), yes); err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			if err := cli.DeleteGovernancePolicy(ctx, token.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted governance policy for token %q (id %d)\n", clean(policy.TokenName), token.ID)
			return nil
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newGovernanceUsageCmd() *cobra.Command {
	var f connFlags
	var outputFormat string
	c := &cobra.Command{
		Use:   "usage <token-id|name>",
		Short: "Show query-governance usage for a token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			token, err := resolveTokenRef(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			usage, policy, err := cli.GetGovernanceUsage(ctx, token.ID)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), struct {
					Usage  *client.GovernanceUsage  `json:"usage"`
					Policy *client.GovernancePolicy `json:"policy"`
				}{usage, policy})
			}
			writeGovernanceUsageInfo(cmd.OutOrStdout(), usage, policy)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func renderGovernancePolicyList(w io.Writer, policies []client.GovernancePolicy, noHeader, asCSV bool) error {
	sorted := append([]client.GovernancePolicy(nil), policies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TokenID < sorted[j].TokenID })
	if asCSV {
		cw := csv.NewWriter(w)
		if !noHeader {
			if err := cw.Write([]string{"token_id", "token_name", "rate_limit_per_minute", "rate_limit_per_hour", "max_queries_per_hour", "max_queries_per_day", "max_rows_per_query", "max_scan_duration_sec", "created_at", "updated_at"}); err != nil {
				return err
			}
		}
		for _, p := range sorted {
			if err := cw.Write(governancePolicyCSVRow(p)); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	if len(sorted) == 0 {
		_, err := fmt.Fprintln(w, "(no governance policies)")
		return err
	}
	rows := make([][]string, 0, len(sorted))
	for _, p := range sorted {
		rows = append(rows, []string{
			strconv.FormatInt(p.TokenID, 10), clean(p.TokenName), governanceLimit(p.RateLimitPerMinute), governanceLimit(p.RateLimitPerHour),
			governanceLimit(p.MaxQueriesPerHour), governanceLimit(p.MaxQueriesPerDay), governanceLimit(p.MaxRowsPerQuery), governanceLimit(p.MaxScanDurationSec),
		})
	}
	headers := []string{"TOKEN ID", "TOKEN", "PER MIN", "PER HOUR", "QUERIES/HOUR", "QUERIES/DAY", "ROWS/QUERY", "SCAN SECONDS"}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}

func governancePolicyCSVRow(p client.GovernancePolicy) []string {
	return []string{
		strconv.FormatInt(p.TokenID, 10), p.TokenName, strconv.Itoa(p.RateLimitPerMinute), strconv.Itoa(p.RateLimitPerHour),
		strconv.Itoa(p.MaxQueriesPerHour), strconv.Itoa(p.MaxQueriesPerDay), strconv.Itoa(p.MaxRowsPerQuery), strconv.Itoa(p.MaxScanDurationSec),
		fmtServerTime(p.CreatedAt, ""), fmtServerTime(p.UpdatedAt, ""),
	}
}

func governanceLimit(value int) string {
	if value == 0 {
		return "unlimited"
	}
	return strconv.Itoa(value)
}

func writeGovernancePolicyInfo(w io.Writer, p *client.GovernancePolicy) {
	fmt.Fprintf(w, "token id:               %d\n", p.TokenID)
	fmt.Fprintf(w, "token:                  %s\n", clean(p.TokenName))
	fmt.Fprintf(w, "rate limit per minute:  %s\n", governanceLimit(p.RateLimitPerMinute))
	fmt.Fprintf(w, "rate limit per hour:    %s\n", governanceLimit(p.RateLimitPerHour))
	fmt.Fprintf(w, "max queries per hour:   %s\n", governanceLimit(p.MaxQueriesPerHour))
	fmt.Fprintf(w, "max queries per day:    %s\n", governanceLimit(p.MaxQueriesPerDay))
	fmt.Fprintf(w, "max rows per query:     %s\n", governanceLimit(p.MaxRowsPerQuery))
	fmt.Fprintf(w, "max scan duration:      %s sec\n", governanceLimit(p.MaxScanDurationSec))
	fmt.Fprintf(w, "created:                %s\n", fmtServerTime(p.CreatedAt, "-"))
	fmt.Fprintf(w, "updated:                %s\n", fmtServerTime(p.UpdatedAt, "-"))
}

func writeGovernanceUsageInfo(w io.Writer, usage *client.GovernanceUsage, policy *client.GovernancePolicy) {
	fmt.Fprintf(w, "token id:                       %d\n", usage.TokenID)
	fmt.Fprintf(w, "queries this hour:              %d\n", usage.QueriesThisHour)
	fmt.Fprintf(w, "queries this day:               %d\n", usage.QueriesThisDay)
	fmt.Fprintf(w, "hour resets:                    %s\n", fmtServerTime(usage.HourResetAt, "-"))
	fmt.Fprintf(w, "day resets:                     %s\n", fmtServerTime(usage.DayResetAt, "-"))
	fmt.Fprintf(w, "remaining rate limit per minute: %d\n", usage.RateLimitRemainingPerMin)
	fmt.Fprintf(w, "remaining rate limit per hour:   %d\n", usage.RateLimitRemainingPerHour)
	if policy == nil {
		fmt.Fprintln(w, "policy:                         none")
		return
	}
	fmt.Fprintf(w, "policy rate limit per minute:   %s\n", governanceLimit(policy.RateLimitPerMinute))
	fmt.Fprintf(w, "policy rate limit per hour:     %s\n", governanceLimit(policy.RateLimitPerHour))
	fmt.Fprintf(w, "policy max queries per hour:    %s\n", governanceLimit(policy.MaxQueriesPerHour))
	fmt.Fprintf(w, "policy max queries per day:     %s\n", governanceLimit(policy.MaxQueriesPerDay))
	fmt.Fprintf(w, "policy max rows per query:      %s\n", governanceLimit(policy.MaxRowsPerQuery))
	fmt.Fprintf(w, "policy max scan duration:       %s sec\n", governanceLimit(policy.MaxScanDurationSec))
}
