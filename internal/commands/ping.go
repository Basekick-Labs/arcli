// ping: connectivity + auth sanity check. The first thing an operator
// runs after `config create`, and the first thing to run when "arcli
// doesn't work".
package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

// pingReport is the `-o json` shape. It is always emitted, on failure
// too, so scripts can inspect which half failed.
type pingReport struct {
	Connection string      `json:"connection"`
	Endpoint   string      `json:"endpoint"`
	Health     *pingHealth `json:"health"`
	Auth       *pingAuth   `json:"auth"`
	Error      string      `json:"error,omitempty"`
}

type pingHealth struct {
	Status    string          `json:"status"`
	Uptime    string          `json:"uptime,omitempty"`
	LatencyMs float64         `json:"latency_ms"`
	Storage   json.RawMessage `json:"storage,omitempty"`
	License   json.RawMessage `json:"license,omitempty"`
}

type pingAuth struct {
	Valid    bool              `json:"valid"`
	Disabled bool              `json:"disabled,omitempty"`
	Token    *client.TokenInfo `json:"token,omitempty"`
	Error    string            `json:"error,omitempty"`
}

func newPingCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "ping",
		Short: "Check that the Arc endpoint is reachable and the token authenticates",
		Long: `Check connectivity and authentication for the resolved connection.

Two requests: GET /health (public, no token sent) and
GET /api/v1/auth/verify (with the token). Exit status is 0 only when the
endpoint answered AND the token is valid or the server has
authentication disabled. Any failure exits 1; with -o json the report is
still printed so the failing half can be read.`,
		Example: `  arcli ping
  arcli ping -c prod
  arcli ping --endpoint http://localhost:8000 --token T -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			cli, connName, err := f.client(cmd)
			if err != nil {
				return err
			}
			rep := pingReport{Connection: connName, Endpoint: cli.Endpoint()}
			var failure error

			// Separate deadlines: a slow /health must not eat the
			// budget of /verify and get reported as an auth failure.
			hctx, hcancel := f.ctx(cmd)
			defer hcancel()
			h, err := cli.Health(hctx)
			if err != nil {
				failure = fmt.Errorf("health check failed: %w", err)
				rep.Error = failure.Error()
				rep.Auth = &pingAuth{Valid: false, Error: "not attempted"}
			} else {
				rep.Health = &pingHealth{
					Status: h.Status, Uptime: h.Uptime,
					LatencyMs: float64(h.Latency.Microseconds()) / 1000,
					Storage:   h.Storage, License: h.License,
				}
				vctx, vcancel := f.ctx(cmd)
				defer vcancel()
				me, verr := cli.Verify(vctx)
				var he *client.HTTPError
				switch {
				case verr == nil:
					rep.Auth = &pingAuth{Valid: true, Token: me}
				case errors.As(verr, &he) && he.Status == http.StatusNotFound:
					// /api/v1/auth/* is only registered when auth is
					// enabled server-side; 404 means "no auth", not "bad token".
					rep.Auth = &pingAuth{Valid: false, Disabled: true}
				default:
					rep.Auth = &pingAuth{Valid: false, Error: verr.Error()}
					failure = fmt.Errorf("authentication failed: %w", verr)
					rep.Error = failure.Error()
				}
			}

			if outputFormat == output.FormatJSON {
				if err := writeJSON(cmd.OutOrStdout(), rep); err != nil {
					return err
				}
				return failure
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "endpoint:   %s (connection %s)\n", rep.Endpoint, rep.Connection)
			if rep.Health == nil {
				fmt.Fprintf(w, "health:     FAILED\n")
				return failure
			}
			fmt.Fprintf(w, "health:     %s (uptime %s, latency %.1fms)%s\n", rep.Health.Status, rep.Health.Uptime, rep.Health.LatencyMs, summariseStorage(rep.Health.Storage))
			switch {
			case rep.Auth.Disabled:
				fmt.Fprintf(w, "auth:       disabled on server (no token required)\n")
			case rep.Auth.Valid:
				t := rep.Auth.Token
				line := fmt.Sprintf("auth:       ok as %q (id %d, %s)", t.Name, t.ID, permsOrNone(t.Permissions))
				if !hasPermission(t.Permissions, "admin") {
					line += " — not admin; `auth token` commands will be refused"
				}
				fmt.Fprintln(w, line)
			default:
				fmt.Fprintf(w, "auth:       FAILED (%s)\n", rep.Auth.Error)
			}
			return failure
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// summariseStorage turns the /health storage map into " storage:
// hot=ok cold=degraded" for the table line; empty when absent.
func summariseStorage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var tiers map[string]struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &tiers); err != nil || len(tiers) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tiers))
	for _, name := range sortedKeys(tiers) {
		parts = append(parts, name+"="+tiers[name].State)
	}
	return " storage: " + strings.Join(parts, " ")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func hasPermission(perms []string, want string) bool {
	for _, p := range perms {
		if p == want {
			return true
		}
	}
	return false
}
