package commands

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

const maxMQTTConfigSize = 1 << 20

func newMQTTCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "mqtt",
		Short: "Inspect and manage Arc MQTT subscriptions",
		Long: `Inspect MQTT health and manage subscriptions through Arc's MQTT API.
All MQTT endpoints require an admin token. Create and update requests read JSON
from a file or stdin so broker credentials do not need to be passed as flags.`,
	}
	c.AddCommand(newMQTTHealthCmd(), newMQTTStatsCmd(), newMQTTSubscriptionCmd())
	return c
}

func newMQTTHealthCmd() *cobra.Command {
	var f connFlags
	var outputFormat string
	c := &cobra.Command{
		Use:   "health",
		Short: "Show MQTT subsystem health",
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
			h, err := cli.MQTTHealth(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), h.Raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "status:   %s\nhealthy:  %t\n", clean(h.Status), h.Healthy)
			if h.Error != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "error:    %s\n", clean(h.Error))
			}
			if h.Subscriptions != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "subscriptions: %d total, %d running, %d stopped, %d errors\n",
					h.Subscriptions.Total, h.Subscriptions.Running, h.Subscriptions.Stopped, h.Subscriptions.Errors)
			}
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newMQTTStatsCmd() *cobra.Command {
	var f connFlags
	var outputFormat string
	c := &cobra.Command{
		Use:   "stats",
		Short: "Show aggregate MQTT subscription statistics",
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
			stats, err := cli.MQTTStats(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeRawJSON(cmd.OutOrStdout(), stats.Raw)
			}
			writeMQTTStatsSummary(cmd.OutOrStdout(), stats.Summary)
			if len(stats.Subscriptions) == 0 {
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout())
			return renderMQTTStats(cmd.OutOrStdout(), stats.Subscriptions, false)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newMQTTSubscriptionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "subscription",
		Aliases: []string{"subscriptions"},
		Short:   "Manage MQTT subscriptions",
	}
	c.AddCommand(
		newMQTTSubscriptionListCmd(),
		newMQTTSubscriptionShowCmd(),
		newMQTTSubscriptionCreateCmd(),
		newMQTTSubscriptionUpdateCmd(),
		newMQTTSubscriptionDeleteCmd(),
		newMQTTSubscriptionActionCmd("start", "Start an MQTT subscription", false),
		newMQTTSubscriptionActionCmd("stop", "Stop an MQTT subscription", true),
		newMQTTSubscriptionActionCmd("pause", "Pause an MQTT subscription", true),
		newMQTTSubscriptionActionCmd("restart", "Restart an MQTT subscription", true),
		newMQTTSubscriptionStatsCmd(),
	)
	return c
}

func newMQTTSubscriptionListCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		noHeader     bool
	)
	c := &cobra.Command{
		Use:   "list",
		Short: "List MQTT subscriptions",
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
			list, err := cli.ListMQTTSubscriptions(ctx)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), safeMQTTSubscriptions(list))
			}
			return renderMQTTSubscriptionList(cmd.OutOrStdout(), list, noHeader, outputFormat == output.FormatCSV)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(&noHeader, "no-header", false, "suppress column header row (table + csv)")
	return c
}

func newMQTTSubscriptionShowCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one MQTT subscription",
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
			sub, err := cli.GetMQTTSubscription(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), safeMQTTSubscription(*sub))
			}
			writeMQTTSubscriptionInfo(cmd.OutOrStdout(), sub)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newMQTTSubscriptionCreateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		configFile   string
		configStdin  bool
	)
	c := &cobra.Command{
		Use:   "create",
		Short: "Create an MQTT subscription from a JSON config file or stdin",
		Long: `Create an MQTT subscription from JSON. Broker credentials belong in the
JSON input, not command-line arguments. Use --config-file with a file readable
only by you (chmod 600), or --config-stdin to read from a pipe.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			req := client.MQTTSubscriptionCreateRequest{}
			if err := readMQTTConfig(cmd, configFile, configStdin, &req, func() bool {
				return req.Username != "" || req.Password != "" || mqttBrokerHasCredentials(req.Broker)
			}); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			sub, err := cli.CreateMQTTSubscription(ctx, req)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), safeMQTTSubscription(*sub))
			}
			writeMQTTSubscriptionInfo(cmd.OutOrStdout(), sub)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVar(&configFile, "config-file", "", "JSON subscription config path; protect files containing credentials with chmod 600")
	c.Flags().BoolVar(&configStdin, "config-stdin", false, "read the JSON subscription config from stdin")
	c.MarkFlagsMutuallyExclusive("config-file", "config-stdin")
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newMQTTSubscriptionUpdateCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
		configFile   string
		configStdin  bool
	)
	c := &cobra.Command{
		Use:   "update <id>",
		Short: "Update an MQTT subscription from a JSON config file or stdin",
		Long: `Update an MQTT subscription from a partial JSON object. Broker credentials
belong in the JSON input, not command-line arguments. Use --config-file with a
file readable only by you (chmod 600), or --config-stdin to read from a pipe.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(outputFormat) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", outputFormat)
			}
			req := client.MQTTSubscriptionUpdateRequest{}
			if err := readMQTTConfig(cmd, configFile, configStdin, &req, func() bool {
				return req.Username != nil || req.Password != nil || (req.Broker != nil && mqttBrokerHasCredentials(*req.Broker))
			}); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			sub, err := cli.UpdateMQTTSubscription(ctx, args[0], req)
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), safeMQTTSubscription(*sub))
			}
			writeMQTTSubscriptionInfo(cmd.OutOrStdout(), sub)
			return nil
		},
	}
	f.add(c)
	c.Flags().StringVar(&configFile, "config-file", "", "JSON subscription config path; protect files containing credentials with chmod 600")
	c.Flags().BoolVar(&configStdin, "config-stdin", false, "read the JSON subscription config from stdin")
	c.MarkFlagsMutuallyExclusive("config-file", "config-stdin")
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func newMQTTSubscriptionDeleteCmd() *cobra.Command {
	var (
		f   connFlags
		yes bool
	)
	c := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete an MQTT subscription",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete MQTT subscription %s?", args[0]), yes); err != nil {
				return err
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			if err := cli.DeleteMQTTSubscription(ctx, args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted MQTT subscription %s\n", clean(args[0]))
			return nil
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newMQTTSubscriptionActionCmd(action, short string, confirm bool) *cobra.Command {
	var (
		f   connFlags
		yes bool
	)
	c := &cobra.Command{
		Use:   action + " <id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if confirm {
				if err := confirmOrAbort(cmd, fmt.Sprintf("%s MQTT subscription %s?", strings.ToUpper(action[:1])+action[1:], args[0]), yes); err != nil {
					return err
				}
			}
			cli, _, err := f.client(cmd)
			if err != nil {
				return err
			}
			ctx, cancel := f.ctx(cmd)
			defer cancel()
			message, err := cli.MQTTSubscriptionAction(ctx, args[0], action)
			if err != nil {
				return err
			}
			if message == "" {
				message = map[string]string{
					"start": "Started", "stop": "Stopped", "pause": "Paused", "restart": "Restarted",
				}[action] + " MQTT subscription " + args[0]
			}
			fmt.Fprintln(cmd.OutOrStdout(), clean(message))
			return nil
		},
	}
	f.add(c)
	if confirm {
		c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	}
	return c
}

func newMQTTSubscriptionStatsCmd() *cobra.Command {
	var (
		f            connFlags
		outputFormat string
	)
	c := &cobra.Command{
		Use:   "stats <id>",
		Short: "Show runtime statistics for one MQTT subscription",
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
			stats, err := cli.MQTTSubscriptionStats(ctx, args[0])
			if err != nil {
				return err
			}
			if outputFormat == output.FormatJSON {
				return writeJSON(cmd.OutOrStdout(), stats)
			}
			return renderMQTTStats(cmd.OutOrStdout(), []client.MQTTSubscriptionStats{*stats}, true)
		},
	}
	f.add(c)
	c.Flags().StringVarP(&outputFormat, "output", "o", output.FormatTable, "output format: table|json")
	return c
}

func readMQTTConfig(cmd *cobra.Command, path string, fromStdin bool, target any, containsCredentials func() bool) error {
	var reader io.Reader
	var fileMode os.FileMode
	if fromStdin {
		reader = cmd.InOrStdin()
	} else {
		if path == "" {
			return fmt.Errorf("provide --config-file or --config-stdin")
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open subscription config: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect subscription config: %w", err)
		}
		fileMode = info.Mode().Perm()
		reader = file
	}

	data, err := io.ReadAll(io.LimitReader(reader, maxMQTTConfigSize+1))
	if err != nil {
		return fmt.Errorf("read subscription config: %w", err)
	}
	if len(data) > maxMQTTConfigSize {
		return fmt.Errorf("subscription config exceeds %d bytes", maxMQTTConfigSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return fmt.Errorf("decode subscription config: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode subscription config: expected one JSON object")
		}
		return fmt.Errorf("decode subscription config: %w", err)
	}
	if !fromStdin && containsCredentials() && fileMode&0o077 != 0 {
		return fmt.Errorf("subscription config contains broker credentials but is accessible by group or others; run chmod 600 %q or use --config-stdin", path)
	}
	return nil
}

func writeMQTTStatsSummary(w io.Writer, s client.MQTTStatsSummary) {
	fmt.Fprintf(w, "subscriptions: %d total, %d running, %d stopped, %d errors\n", s.TotalSubscriptions, s.Running, s.Stopped, s.Error)
	fmt.Fprintf(w, "messages:      %d received, %d failed\n", s.TotalMessages, s.TotalFailed)
	fmt.Fprintf(w, "bytes received: %s\n", humanBytes(s.TotalBytes))
}

func renderMQTTStats(w io.Writer, stats []client.MQTTSubscriptionStats, object bool) error {
	if object {
		if len(stats) == 0 {
			return nil
		}
		s := stats[0]
		fmt.Fprintf(w, "id:                %s\n", clean(s.ID))
		fmt.Fprintf(w, "name:              %s\n", clean(s.Name))
		fmt.Fprintf(w, "status:            %s\n", clean(s.Status))
		fmt.Fprintf(w, "messages received: %d\n", s.MessagesReceived)
		fmt.Fprintf(w, "messages failed:   %d\n", s.MessagesFailed)
		fmt.Fprintf(w, "bytes received:    %s\n", humanBytes(s.BytesReceived))
		fmt.Fprintf(w, "reconnects:        %d\n", s.Reconnects)
		fmt.Fprintf(w, "last message:      %s\n", fmtMQTTTime(s.LastMessageAt))
		fmt.Fprintf(w, "connected since:   %s\n", fmtMQTTTime(s.ConnectedSince))
		return nil
	}
	rows := make([][]string, 0, len(stats))
	for _, s := range stats {
		rows = append(rows, []string{
			clean(s.ID), clean(s.Name), clean(s.Status), fmt.Sprint(s.MessagesReceived),
			fmt.Sprint(s.MessagesFailed), humanBytes(s.BytesReceived), fmt.Sprint(s.Reconnects),
			fmtMQTTTime(s.LastMessageAt), fmtMQTTTime(s.ConnectedSince),
		})
	}
	return output.Table(w, []string{"ID", "NAME", "STATUS", "RECEIVED", "FAILED", "BYTES", "RECONNECTS", "LAST MESSAGE", "CONNECTED SINCE"}, rows)
}

func renderMQTTSubscriptionList(w io.Writer, list []client.MQTTSubscription, noHeader, asCSV bool) error {
	if len(list) == 0 && !asCSV {
		_, err := fmt.Fprintln(w, "(no MQTT subscriptions)")
		return err
	}
	rows := make([][]string, 0, len(list))
	for _, sub := range list {
		rows = append(rows, mqttSubscriptionRow(sub))
	}
	headers := []string{"ID", "NAME", "STATUS", "BROKER", "CLIENT ID", "TOPICS", "QOS", "DATABASE", "TLS", "AUTO START", "PASSWORD"}
	if asCSV {
		cw := csv.NewWriter(w)
		if !noHeader {
			if err := cw.Write([]string{"id", "name", "status", "broker", "client_id", "topics", "qos", "database", "tls", "auto_start", "password"}); err != nil {
				return err
			}
		}
		for _, row := range rows {
			if err := cw.Write(row); err != nil {
				return err
			}
		}
		cw.Flush()
		return cw.Error()
	}
	if noHeader {
		headers = nil
	}
	return output.Table(w, headers, rows)
}

func mqttSubscriptionRow(s client.MQTTSubscription) []string {
	password := "no"
	if s.HasPassword {
		password = "set"
	}
	return []string{
		clean(s.ID), clean(s.Name), clean(s.Status), scrubMQTTBroker(s.Broker), clean(s.ClientID),
		clean(strings.Join(s.Topics, ",")), fmt.Sprint(s.QoS), clean(s.Database),
		fmt.Sprint(s.TLSEnabled), fmt.Sprint(s.AutoStart), password,
	}
}

func writeMQTTSubscriptionInfo(w io.Writer, sub *client.MQTTSubscription) {
	fmt.Fprintf(w, "id:                       %s\n", clean(sub.ID))
	fmt.Fprintf(w, "name:                     %s\n", clean(sub.Name))
	fmt.Fprintf(w, "status:                   %s\n", clean(sub.Status))
	fmt.Fprintf(w, "broker:                   %s\n", scrubMQTTBroker(sub.Broker))
	fmt.Fprintf(w, "client id:                %s\n", clean(sub.ClientID))
	fmt.Fprintf(w, "topics:                   %s\n", clean(strings.Join(sub.Topics, ", ")))
	fmt.Fprintf(w, "qos:                      %d\n", sub.QoS)
	fmt.Fprintf(w, "database:                 %s\n", clean(sub.Database))
	fmt.Fprintf(w, "tls enabled:              %t\n", sub.TLSEnabled)
	fmt.Fprintf(w, "tls insecure skip verify: %t\n", sub.TLSInsecureSkipVerify)
	fmt.Fprintf(w, "auto start:               %t\n", sub.AutoStart)
	fmt.Fprintf(w, "password:                 %s\n", map[bool]string{true: "set", false: "not set"}[sub.HasPassword])
	if sub.ErrorMessage != "" {
		fmt.Fprintf(w, "error:                    %s\n", clean(sub.ErrorMessage))
	}
	if sub.TopicMapping != nil {
		keys := make([]string, 0, len(sub.TopicMapping))
		for key := range sub.TopicMapping {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(w, "topic mapping %s: %s\n", clean(key), clean(sub.TopicMapping[key]))
		}
	}
	fmt.Fprintf(w, "keep alive:               %ds\n", sub.KeepAliveSeconds)
	fmt.Fprintf(w, "connect timeout:          %ds\n", sub.ConnectTimeoutSeconds)
	fmt.Fprintf(w, "reconnect max:            %ds\n", sub.ReconnectMaxSeconds)
	fmt.Fprintf(w, "clean session:            %t\n", sub.CleanSession)
	fmt.Fprintf(w, "created:                  %s\n", fmtMQTTTimeValue(sub.CreatedAt))
	fmt.Fprintf(w, "updated:                  %s\n", fmtMQTTTimeValue(sub.UpdatedAt))
}

func safeMQTTSubscriptions(list []client.MQTTSubscription) []client.MQTTSubscription {
	out := make([]client.MQTTSubscription, len(list))
	for i, sub := range list {
		out[i] = safeMQTTSubscription(sub)
	}
	return out
}

func safeMQTTSubscription(sub client.MQTTSubscription) client.MQTTSubscription {
	sub.Broker = scrubMQTTBroker(sub.Broker)
	sub.Username = ""
	return sub
}

func scrubMQTTBroker(broker string) string {
	u, err := url.Parse(broker)
	if err != nil {
		if strings.Contains(broker, "@") {
			return "<redacted>"
		}
		return clean(broker)
	}
	if u.User != nil {
		u.User = nil
	}
	return clean(u.String())
}

func mqttBrokerHasCredentials(broker string) bool {
	u, err := url.Parse(broker)
	if err != nil {
		return strings.Contains(broker, "@")
	}
	return u.User != nil
}

func fmtMQTTTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func fmtMQTTTimeValue(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
