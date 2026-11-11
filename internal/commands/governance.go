// governance commands manage enterprise query-governance policies and usage.
package commands

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
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

The governance endpoints require an Enterprise licence with query governance
enabled, and an admin token when authentication is enabled. A limit of 0 means unlimited.
Policies and counters are per-node and are not replicated. Configure each node
separately; behind a load balancer, aggregate quotas can be N times the limit.
Displayed timestamps use UTC RFC3339 by default; --time-output rfc3339nano
shows fractional seconds. JSON retains the available timestamp precision. Unavailable routes retain
the server HTTP error without inferring configuration or license causes.`,
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
		var httpErr *client.HTTPError
		id, parseErr := strconv.ParseInt(ref, 10, 64)
		if parseErr != nil || id <= 0 || !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound {
			return nil, nil, err
		}
		// Deleted tokens can leave policies behind; a numeric ID remains usable.
		token = &client.TokenInfo{ID: id}
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
	var to timeOutput
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
			// Arc does not populate token_name. Enrichment is best-effort so
			// orphan policies and restricted token-list access remain usable.
			if tokens, err := cli.ListTokens(ctx); err == nil {
				names := make(map[int64]string, len(tokens))
				for _, token := range tokens {
					names[token.ID] = token.Name
				}
				for i := range policies {
					if name, ok := names[policies[i].TokenID]; ok {
						policies[i].TokenName = name
					}
				}
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), struct {
					Policies []client.GovernancePolicy `json:"policies"`
					Count    int                       `json:"count"`
				}{policies, len(policies)})
			}
			return renderGovernancePolicyList(cmd.OutOrStdout(), policies, noHeader, outputFormat == output.FormatCSV, to)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	to.add(c)
	return c
}

func newGovernancePolicyShowCmd() *cobra.Command {
	var f connFlags
	var to timeOutput
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
			writeGovernancePolicyInfo(cmd.OutOrStdout(), policy, to)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	to.add(c)
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
	var to timeOutput
	var outputFormat string
	var limits governanceLimitFlags
	var unlimited bool
	c := &cobra.Command{
		Use:   "create <token-id|name>",
		Short: "Create a query-governance policy",
		Long: `Create a per-node policy. Omitted limits are unlimited and override server defaults.
Pass at least one limit flag, or --unlimited to explicitly disable every limit.`,
		Example: `  arcli governance policy create batch --rate-limit-per-minute 30 --max-queries-per-day 5000
  arcli governance policy create 12 --max-rows-per-query 10000 --max-scan-duration-sec 60
  arcli governance policy create 12 --unlimited`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if !governanceLimitFlagsChanged(cmd) && !unlimited {
				return fmt.Errorf("pass at least one governance limit flag or --unlimited (omitted limits override server defaults with unlimited)")
			}
			if unlimited && governanceLimitFlagsChanged(cmd) {
				return fmt.Errorf("--unlimited cannot be combined with limit flags")
			}
			if err := client.ValidateGovernanceLimits(limits.request(0)); err != nil {
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
			req := limits.request(token.ID)
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			existing, err := cli.GetGovernancePolicy(ctx, token.ID)
			if err == nil && existing.ID != 0 {
				return fmt.Errorf("governance policy already exists for token %q (id %d); use `arcli governance policy update`", clean(token.Name), token.ID)
			}
			if err != nil {
				var httpErr *client.HTTPError
				if !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound {
					return err
				}
			}
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
			writeGovernancePolicyInfo(cmd.OutOrStdout(), policy, to)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	to.add(c)
	limits.add(c)
	c.Flags().BoolVar(&unlimited, "unlimited", false, "explicitly create a policy with every limit unlimited")
	return c
}

func newGovernancePolicyUpdateCmd() *cobra.Command {
	var f connFlags
	var to timeOutput
	var outputFormat string
	var limits governanceLimitFlags
	c := &cobra.Command{
		Use:   "update <token-id|name>",
		Short: "Update selected limits on a token's query-governance policy",
		Long: `Update one or more policy limits. Arc replaces the complete policy on PUT,
so arcli reads the current policy and merges only the flags you pass.
Arc has no PATCH or version check: concurrent updates can lose changes.
Policies apply only to the contacted node and are not replicated.`,
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
			validation := limits.request(0)
			if err := client.ValidateGovernanceLimits(validation); err != nil {
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
			fmt.Fprintf(cmd.OutOrStdout(), "Updated governance policy for %s\n", governanceTokenLabel(token))
			writeGovernancePolicyInfo(cmd.OutOrStdout(), updated, to)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	to.add(c)
	limits.add(c)
	return c
}

func newGovernancePolicyDeleteCmd() *cobra.Command {
	var f connFlags
	var yes bool
	c := &cobra.Command{
		Use:   "delete <token-id|name>",
		Short: "Delete a token's query-governance policy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			token, _, err := resolveGovernancePolicyRef(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete governance policy for %s?", governanceTokenLabel(token)), yes); err != nil {
				return err
			}
			ctx, cancel = f.ctx(cmd)
			defer cancel()
			if err := cli.DeleteGovernancePolicy(ctx, token.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted governance policy for %s\n", governanceTokenLabel(token))
			return nil
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newGovernanceUsageCmd() *cobra.Command {
	var f connFlags
	var to timeOutput
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
			writeGovernanceUsageInfo(cmd.OutOrStdout(), usage, policy, to)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	to.add(c)
	return c
}

func renderGovernancePolicyList(w io.Writer, policies []client.GovernancePolicy, noHeader, asCSV bool, to timeOutput) error {
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
			if err := cw.Write(governancePolicyCSVRow(p, to)); err != nil {
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

func governancePolicyCSVRow(p client.GovernancePolicy, to timeOutput) []string {
	return []string{
		strconv.FormatInt(p.TokenID, 10), p.TokenName, strconv.Itoa(p.RateLimitPerMinute), strconv.Itoa(p.RateLimitPerHour),
		strconv.Itoa(p.MaxQueriesPerHour), strconv.Itoa(p.MaxQueriesPerDay), strconv.Itoa(p.MaxRowsPerQuery), strconv.Itoa(p.MaxScanDurationSec),
		to.Server(p.CreatedAt, ""), to.Server(p.UpdatedAt, ""),
	}
}

func governanceLimit(value int) string {
	if value == 0 {
		return "unlimited"
	}
	return strconv.Itoa(value)
}

func writeGovernancePolicyInfo(w io.Writer, p *client.GovernancePolicy, to timeOutput) {
	fmt.Fprintf(w, "token id:               %d\n", p.TokenID)
	tokenName := clean(p.TokenName)
	if tokenName == "" {
		tokenName = "- (token not found)"
	}
	fmt.Fprintf(w, "token:                  %s\n", tokenName)
	fmt.Fprintf(w, "rate limit per minute:  %s\n", governanceLimit(p.RateLimitPerMinute))
	fmt.Fprintf(w, "rate limit per hour:    %s\n", governanceLimit(p.RateLimitPerHour))
	fmt.Fprintf(w, "max queries per hour:   %s\n", governanceLimit(p.MaxQueriesPerHour))
	fmt.Fprintf(w, "max queries per day:    %s\n", governanceLimit(p.MaxQueriesPerDay))
	fmt.Fprintf(w, "max rows per query:     %s\n", governanceLimit(p.MaxRowsPerQuery))
	fmt.Fprintf(w, "max scan duration:      %s\n", governanceDuration(p.MaxScanDurationSec))
	fmt.Fprintf(w, "created:                %s\n", to.Server(p.CreatedAt, "-"))
	fmt.Fprintf(w, "updated:                %s\n", to.Server(p.UpdatedAt, "-"))
}

// governanceTokenLabel names a token in messages. A policy reached by the
// numeric ID of a deleted token has no name.
func governanceTokenLabel(token *client.TokenInfo) string {
	if token.Name == "" {
		return fmt.Sprintf("token id %d (token not found)", token.ID)
	}
	return fmt.Sprintf("token %q (id %d)", clean(token.Name), token.ID)
}

func governanceDuration(value int) string {
	if value == 0 {
		return "unlimited"
	}
	return strconv.Itoa(value) + " sec"
}

func governanceUsageValue(value, limit int, absent string) string {
	if limit <= 0 {
		return absent
	}
	return strconv.Itoa(value)
}

func writeGovernanceUsageInfo(w io.Writer, usage *client.GovernanceUsage, policy *client.GovernancePolicy, to timeOutput) {
	fmt.Fprintf(w, "token id:                       %d\n", usage.TokenID)
	// A nil policy means no token policy and no server defaults: the token is
	// unrestricted. Arc keeps one quota tracker that counts both the hour and
	// the day once either quota is set, so both counters are real then.
	limits := client.GovernancePolicy{}
	if policy != nil {
		limits = *policy
	}
	quota := max(limits.MaxQueriesPerHour, limits.MaxQueriesPerDay)
	fmt.Fprintf(w, "queries this hour:              %s\n", governanceUsageValue(usage.QueriesThisHour, quota, "n/a (no query quota)"))
	fmt.Fprintf(w, "queries this day:               %s\n", governanceUsageValue(usage.QueriesThisDay, quota, "n/a (no query quota)"))
	fmt.Fprintf(w, "hour resets:                    %s\n", to.Server(usage.HourResetAt, "-"))
	fmt.Fprintf(w, "day resets:                     %s\n", to.Server(usage.DayResetAt, "-"))
	fmt.Fprintf(w, "remaining rate limit per minute: %s\n", governanceUsageValue(usage.RateLimitRemainingPerMin, limits.RateLimitPerMinute, "unlimited"))
	fmt.Fprintf(w, "remaining rate limit per hour:   %s\n", governanceUsageValue(usage.RateLimitRemainingPerHour, limits.RateLimitPerHour, "unlimited"))
	if policy == nil {
		fmt.Fprintln(w, "policy:                         none (no token policy or server defaults; no limits apply)")
		return
	}
	if policy.ID == 0 {
		fmt.Fprintln(w, "policy:                         server defaults (no token policy)")
	} else {
		fmt.Fprintln(w, "policy:                         token policy")
	}
	fmt.Fprintln(w, "scope:                          this node only (not replicated)")
	fmt.Fprintf(w, "policy rate limit per minute:   %s\n", governanceLimit(policy.RateLimitPerMinute))
	fmt.Fprintf(w, "policy rate limit per hour:     %s\n", governanceLimit(policy.RateLimitPerHour))
	fmt.Fprintf(w, "policy max queries per hour:    %s\n", governanceLimit(policy.MaxQueriesPerHour))
	fmt.Fprintf(w, "policy max queries per day:     %s\n", governanceLimit(policy.MaxQueriesPerDay))
	fmt.Fprintf(w, "policy max rows per query:      %s\n", governanceLimit(policy.MaxRowsPerQuery))
	fmt.Fprintf(w, "policy max scan duration:       %s\n", governanceDuration(policy.MaxScanDurationSec))
}
