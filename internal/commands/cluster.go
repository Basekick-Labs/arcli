// cluster subcommand: inspect and administer an Arc cluster over
// /api/v1/cluster/*.
//
// Arc registers these routes on every server. On a standalone node
// (no enterprise license, or cluster.enabled=false) each returns
// {"enabled":false,"mode":"standalone","reason":...}; `cluster status`
// reports that as a normal result, every other subcommand treats it as
// an error so scripts never get an empty success.
package commands

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newClusterCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "cluster",
		Short: "Inspect and administer an Arc cluster (Arc Enterprise)",
		Long: `Inspect and administer an Arc cluster (Arc Enterprise clustering).

On a standalone server "cluster status" reports that clustering is
disabled and exits 0; the other subcommands exit 1 with the server's
reason. Node removal requires an admin token and must be sent to the
raft leader.`,
	}
	c.AddCommand(newClusterStatusCmd(), newClusterNodesCmd(), newClusterNodeCmd(), newClusterHealthCmd())
	return c
}

// ---- status ----------------------------------------------------------------

func newClusterStatusCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "status",
		Short: "Show cluster status, raft leader, and node summary",
		Long: `Show cluster status (GET /api/v1/cluster).

On a standalone server this prints "clustering: disabled" with the
server's reason and exits 0. -o json passes the server body through
unchanged in both cases; check "enabled" to branch in scripts.`,
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
			st, err := cli.ClusterStatus(ctx)
			var disabled *client.ClusterDisabledError
			if errors.As(err, &disabled) {
				if outputFormat == output.FormatJSON {
					return writeRawJSON(cmd.OutOrStdout(), disabled.Raw)
				}
				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "clustering:  disabled (%s)\n", clean(disabled.Reason))
				fmt.Fprintf(w, "mode:        standalone\n")
				return nil
			}
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), st.Raw)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "clustering:  enabled\n")
			fmt.Fprintf(w, "cluster:     %s\n", clean(st.ClusterName))
			fmt.Fprintf(w, "running:     %t\n", st.Running)
			fmt.Fprintf(w, "local node:  %s (%s)\n", clean(st.LocalNodeID), clean(st.LocalRole))
			fmt.Fprintf(w, "nodes:       %d total, %d healthy (writers %d, readers %d, compactors %d)\n", st.NodeCount, st.HealthyCount, st.Writers, st.Readers, st.Compactors)
			if st.Raft.Enabled {
				leader := clean(st.Raft.LeaderID)
				if leader == "" {
					leader = "(none elected)"
				}
				fmt.Fprintf(w, "raft:        %s, leader %s, this node is leader: %t\n", clean(st.Raft.State), leader, st.Raft.IsLeader)
			} else {
				fmt.Fprintf(w, "raft:        disabled\n")
			}
			if st.License != nil {
				fmt.Fprintf(w, "license:     %s (valid: %t)\n", clean(st.License.Tier), st.License.Valid)
			}
			if len(st.Nodes) == 0 {
				_, err := fmt.Fprintln(w, "(no nodes)")
				return err
			}
			fmt.Fprintln(w)
			return renderNodeTable(w, st.Nodes, false, false)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- nodes -----------------------------------------------------------------

func newClusterNodesCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
		role, state  string
	)
	c := &cobra.Command{
		Use:   "nodes",
		Short: "List cluster nodes",
		Long: `List cluster nodes (GET /api/v1/cluster/nodes), optionally filtered.

--role:  writer | reader | compactor | standalone
--state: healthy | unhealthy | dead | unknown | joining | leaving

Output formats: table (default) | json | csv`,
		Example: `  arcli cluster nodes
  arcli cluster nodes --state unhealthy -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", outputFormat)
			}
			if role != "" && !containsString(client.ClusterNodeRoles, role) {
				return fmt.Errorf("invalid --role %q (valid: %s)", role, strings.Join(client.ClusterNodeRoles, ", "))
			}
			if state != "" && !containsString(client.ClusterNodeStates, state) {
				return fmt.Errorf("invalid --state %q (valid: %s)", state, strings.Join(client.ClusterNodeStates, ", "))
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			nodes, raw, err := cli.ClusterNodes(ctx, role, state)
			if err != nil {
				return err
			}
			switch outputFormat {
			case output.FormatJSON:
				return writeRawJSON(cmd.OutOrStdout(), raw)
			case output.FormatCSV:
				return renderNodeTable(cmd.OutOrStdout(), nodes, noHeader, true)
			default:
				if len(nodes) == 0 {
					_, err := fmt.Fprintln(cmd.OutOrStdout(), "(no nodes)")
					return err
				}
				return renderNodeTable(cmd.OutOrStdout(), nodes, noHeader, false)
			}
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	c.Flags().StringVar(&role, "role", "", "filter by role")
	c.Flags().StringVar(&state, "state", "", "filter by state")
	return c
}

// ---- node {show, remove} ---------------------------------------------------

func newClusterNodeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "node",
		Short: "Show or remove a single cluster node",
	}
	c.AddCommand(newClusterNodeShowCmd(), newClusterNodeRemoveCmd())
	return c
}

func newClusterNodeShowCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		local        bool
	)
	c := &cobra.Command{
		Use:   "show <id> | --local",
		Short: "Show one cluster node",
		Long: `Show one cluster node (GET /api/v1/cluster/nodes/:id), or with
--local the node this connection is talking to (GET /api/v1/cluster/local),
which additionally reports the node's role capabilities.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			if local == (len(args) == 1) {
				return fmt.Errorf("pass exactly one of a node id or --local")
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			w := cmd.OutOrStdout()
			if local {
				n, raw, err := cli.ClusterLocal(ctx)
				if err != nil {
					return err
				}
				if outputFormat == output.FormatJSON {
					return writeRawJSON(w, raw)
				}
				writeNodeInfo(w, &n.ClusterNode)
				caps := make([]string, 0, len(n.Capabilities))
				for k, v := range n.Capabilities {
					if v {
						caps = append(caps, clean(strings.TrimPrefix(k, "can_")))
					}
				}
				sort.Strings(caps)
				fmt.Fprintf(w, "capabilities:   %s\n", strings.Join(caps, ", "))
				return nil
			}
			n, raw, err := cli.ClusterNode(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, raw)
			}
			writeNodeInfo(w, n)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	c.Flags().BoolVar(&local, "local", false, "show the node this connection is talking to")
	return c
}

func newClusterNodeRemoveCmd() *cobra.Command {
	var (
		f   connFlags
		yes bool
	)
	c := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a node from the cluster (admin, leader only)",
		Long: `Remove a node from the cluster (DELETE /api/v1/cluster/nodes/:id).

Must be sent to the raft leader; a follower refuses with "not the
leader" and arcli then names the leader's API address. The server
refuses to remove the node it is running on (use a graceful shutdown
for that). With raft disabled the removal only affects the registry of
the node you are talking to.

Prompts for confirmation; pass --yes for scripted use. arcli never
retries the removal against another host; when it names the leader,
verify that address against your inventory before re-running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if err := client.ValidateNodeID(id); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			stderr := cmd.ErrOrStderr()

			// Pre-flight: fail fast on standalone, refuse self-removal
			// without a round trip, and learn whether raft is on so the
			// prompt can say what the removal will actually do.
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			st, err := cli.ClusterStatus(ctx)
			if err != nil {
				return err
			}
			if id == st.LocalNodeID {
				return fmt.Errorf("node %q is the node this connection is talking to; the server refuses self-removal — shut it down gracefully instead", clean(id))
			}
			q := fmt.Sprintf("Remove node %q from cluster %q?", clean(id), clean(st.ClusterName))
			if !st.Raft.Enabled {
				q += " Raft is disabled: this only unregisters the node from this server's registry."
			} else if !st.Raft.IsLeader {
				fmt.Fprintf(stderr, "note: this node is not the raft leader (leader: %s); the server will refuse unless leadership changed\n", clean(st.Raft.LeaderID))
			}
			if err := confirmOrAbort(cmd, q, yes); err != nil {
				return err
			}

			ctx, cancel = f.ctx(cmd)
			defer cancel()
			err = cli.RemoveClusterNode(ctx, id)
			if errors.Is(err, client.ErrNotLeader) {
				// Re-fetch: the leader may have changed since pre-flight.
				ctx2, cancel2 := f.ctx(cmd)
				defer cancel2()
				return fmt.Errorf("%w; %s", err, leaderHint(ctx2, cli))
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed node %q from cluster %q\n", clean(id), clean(st.ClusterName))
			return nil
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

// leaderHint resolves raft.leader_id to that node's HTTP api_address
// (never raft.leader_addr, which is the raft transport port) and tells
// the operator where to re-run.
func leaderHint(ctx context.Context, cli *client.Client) string {
	st, err := cli.ClusterStatus(ctx)
	if err != nil {
		return "could not determine the leader (" + err.Error() + "); see `arcli cluster nodes`"
	}
	if st.Raft.LeaderID == "" {
		return "no leader is currently elected; retry shortly"
	}
	scheme := "http"
	if u, perr := url.Parse(cli.Endpoint()); perr == nil && u.Scheme != "" {
		scheme = u.Scheme
	}
	for _, n := range st.Nodes {
		if n.ID == st.Raft.LeaderID && n.APIAddress != "" {
			return fmt.Sprintf("re-run against the leader %q at %s://%s — verify that address against your inventory, then use a connection profile for it (`arcli config create`) or ARC_ENDPOINT/ARC_TOKEN", clean(n.ID), scheme, clean(n.APIAddress))
		}
	}
	return fmt.Sprintf("the leader is node %q; find its API address with `arcli cluster nodes`", clean(st.Raft.LeaderID))
}

// ---- health ----------------------------------------------------------------

func newClusterHealthCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "health",
		Short: "Show cluster health counts and health-checker settings",
		Args:  cobra.NoArgs,
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
			h, err := cli.ClusterHealth(ctx)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if outputFormat == output.FormatJSON {
				return writeRawJSON(w, h.Raw)
			}
			fmt.Fprintf(w, "nodes:            %d total, %d healthy, %d unhealthy\n", h.Total, h.Healthy, h.Unhealthy)
			hc := h.HealthChecker
			fmt.Fprintf(w, "health checker:   running=%t interval=%s timeout=%s unhealthy after %d failed checks\n",
				hc.Running, time.Duration(hc.CheckIntervalMs)*time.Millisecond, time.Duration(hc.CheckTimeoutMs)*time.Millisecond, hc.UnhealthyThreshold)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

// ---- rendering helpers -----------------------------------------------------

// writeRawJSON re-indents the server's body without decoding it into a
// map, so key order and null-vs-absent are exactly what the server sent.
func writeRawJSON(w io.Writer, raw json.RawMessage) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return fmt.Errorf("format server response: %w", err)
	}
	buf.WriteByte('\n')
	_, err := w.Write(buf.Bytes())
	return err
}

// fmtTimeVal renders a time.Time as UTC RFC3339, or "never" for the zero
// value (Arc serialises unset times as 0001-01-01T00:00:00Z).
func fmtTimeVal(t time.Time, none string) string {
	if t.IsZero() {
		return none
	}
	return t.UTC().Format(time.RFC3339)
}

// humanBytes renders a byte count with a binary unit suffix.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func nodeRow(n client.ClusterNode) []string {
	return []string{
		clean(n.ID), clean(n.Name), clean(n.Role), clean(n.State), clean(n.Address), clean(n.APIAddress), clean(n.Version),
		fmtTimeVal(n.LastHeartbeat, "never"), strconv.Itoa(n.FailedChecks),
	}
}

var nodeHeaders = []string{"ID", "NAME", "ROLE", "STATE", "ADDRESS", "API", "VERSION", "LAST HEARTBEAT", "FAILED"}

// renderNodeTable writes nodes sorted by id as a table or CSV.
func renderNodeTable(w io.Writer, nodes []client.ClusterNode, noHeader, asCSV bool) error {
	list := make([]client.ClusterNode, len(nodes))
	copy(list, nodes)
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	if asCSV {
		cw := csv.NewWriter(w)
		if !noHeader {
			if err := cw.Write([]string{"id", "name", "role", "state", "address", "api_address", "version", "last_heartbeat", "failed_checks"}); err != nil {
				return err
			}
		}
		for _, n := range list {
			row := nodeRow(n)
			row[7] = fmtTimeVal(n.LastHeartbeat, "")
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	rows := make([][]string, 0, len(list))
	for _, n := range list {
		rows = append(rows, nodeRow(n))
	}
	headers := nodeHeaders
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}

// writeNodeInfo prints one node as aligned key/value lines.
func writeNodeInfo(w io.Writer, n *client.ClusterNode) {
	fmt.Fprintf(w, "id:             %s\n", clean(n.ID))
	fmt.Fprintf(w, "name:           %s\n", clean(n.Name))
	fmt.Fprintf(w, "role:           %s\n", clean(n.Role))
	fmt.Fprintf(w, "state:          %s\n", clean(n.State))
	fmt.Fprintf(w, "address:        %s\n", clean(n.Address))
	fmt.Fprintf(w, "api address:    %s\n", clean(n.APIAddress))
	if n.ClusterName != "" {
		fmt.Fprintf(w, "cluster:        %s\n", clean(n.ClusterName))
	}
	fmt.Fprintf(w, "version:        %s\n", clean(n.Version))
	fmt.Fprintf(w, "started:        %s\n", fmtTimeVal(n.StartedAt, "-"))
	fmt.Fprintf(w, "joined:         %s\n", fmtTimeVal(n.JoinedAt, "-"))
	fmt.Fprintf(w, "last heartbeat: %s\n", fmtTimeVal(n.LastHeartbeat, "never"))
	fmt.Fprintf(w, "failed checks:  %d\n", n.FailedChecks)
	s := n.Stats
	fmt.Fprintf(w, "stats:          cpu %.1f%%, mem %.1f%%, ingest %d/s, queries %d/s, storage %s, conns %d, active queries %d, compaction jobs %d\n",
		s.CPUUsage, s.MemoryUsage, s.IngestRate, s.QueryRate, humanBytes(s.StorageUsed), s.Connections, s.ActiveQueries, s.CompactionJobs)
}
