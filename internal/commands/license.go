package commands

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/output"
)

func newLicenseCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "license",
		Short: "Inspect the server's license",
	}
	c.AddCommand(newLicenseStatusCmd())
	return c
}

func newLicenseStatusCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the license state reported by Arc",
		Long: `Show the license state from public GET /health. No token is sent and
no authentication check is made. Works on standalone and clustered servers.

Reports tier, status, source, and optional expiry, days remaining, and site-license
state. Expiry uses UTC RFC3339 with fractional seconds when provided. The server's
status is preserved, including active, expired, invalid, and unlicensed.

A missing or null license block means no license is configured (normal for OSS),
which differs from a configured server reporting unlicensed. Every successfully
read license state exits 0; configuration, request, decoding, and output errors
exit nonzero. JSON has configured and license fields; absent configuration uses
{"configured":false,"license":null}. Optional license fields are omitted when
not provided or null. License installation and key/file configuration belong to
the server and are outside this command.`,
		Example: `  arcli license status
  arcli license status -c prod -o json
  arcli license status --endpoint http://localhost:8000 --timeout 5s`,
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
			st, err := cli.LicenseStatus(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeJSON(w, st)
			}
			rows := [][]string{{"configured", strconv.FormatBool(st.Configured)}}
			if lic := st.License; lic != nil {
				rows = append(rows, []string{"tier", clean(lic.Tier)}, []string{"status", clean(lic.Status)}, []string{"source", clean(lic.Source)})
				if lic.ExpiresAt != nil {
					rows = append(rows, []string{"expires_at", lic.ExpiresAt.UTC().Format(time.RFC3339Nano)})
				}
				if lic.DaysRemaining != nil {
					rows = append(rows, []string{"days_remaining", strconv.Itoa(*lic.DaysRemaining)})
				}
				if lic.SiteLicense != nil {
					rows = append(rows, []string{"site_license", strconv.FormatBool(*lic.SiteLicense)})
				}
			} else {
				rows = append(rows, []string{"status", "absent (no license configured)"})
			}
			// tablewriter can swallow destination write errors. Render this
			// small status report first, then propagate errors copying it out.
			var table bytes.Buffer
			if err := output.Table(&table, []string{"FIELD", "VALUE"}, rows); err != nil {
				return err
			}
			_, err = io.Copy(w, &table)
			return err
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}
