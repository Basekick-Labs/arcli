package commands

import (
	"context"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/basekick-labs/arcli/internal/client"
	"github.com/basekick-labs/arcli/internal/output"
)

func newRBACCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rbac",
		Short: "Administer Arc Enterprise RBAC",
		Long:  "Administer organizations, teams, roles, measurement permissions, and token memberships. All commands require an admin token. Arc Enterprise RBAC is required for organization, team, role, and measurement-permission operations and for adding token memberships; listing or removing memberships also works when an existing RBAC configuration remains after a licence lapse.\n\nToken references accept a numeric id or an exact token name. IDs are tried first, so a token named with digits must be addressed by id.",
	}
	c.AddCommand(
		newRBACOrganizationsCmd(),
		newRBACTeamsCmd(),
		newRBACRolesCmd(),
		newRBACMeasurementsCmd(),
		newRBACTokensCmd(),
	)
	return c
}

func rbacClientContext(cmd *cobra.Command, f *connFlags) (*client.Client, context.Context, context.CancelFunc, error) {
	cli, _, err := f.client(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := f.ctx(cmd)
	return cli, ctx, cancel, nil
}

func parseRBACID(raw, kind string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%s id must be a positive integer (got %q)", kind, raw)
	}
	return id, nil
}

func resolveRBACToken(ctx context.Context, cli *client.Client, raw string) (*client.TokenInfo, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("token id or name is required")
	}
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id <= 0 {
		return nil, fmt.Errorf("token id must be a positive integer (got %q)", raw)
	}
	return resolveTokenRef(ctx, cli, raw)
}

func addRBACObjectOutputFlag(c *cobra.Command, format *string) {
	c.Flags().StringVarP(format, "output", "o", output.FormatTable, "output format: table|json")
}

func addRBACListOutputFlags(c *cobra.Command, format *string, noHeader *bool) {
	c.Flags().StringVarP(format, "output", "o", output.FormatTable, "output format: table|json|csv")
	c.Flags().BoolVar(noHeader, "no-header", false, "suppress column header row (table + csv)")
}

func renderRBACList[T any](cmd *cobra.Command, items []T, resource, jsonField string, format string, noHeader bool, headers []string, columns func(T) []string) error {
	switch format {
	case output.FormatJSON:
		return writeJSON(cmd.OutOrStdout(), map[string]any{jsonField: items, "count": len(items)})
	case output.FormatCSV:
		w := csv.NewWriter(cmd.OutOrStdout())
		if !noHeader {
			if err := w.Write(headers); err != nil {
				return err
			}
		}
		for _, item := range items {
			if err := w.Write(columns(item)); err != nil {
				return err
			}
		}
		w.Flush()
		return w.Error()
	default:
		if len(items) == 0 {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "(no %s)\n", resource)
			return err
		}
		rows := make([][]string, 0, len(items))
		for _, item := range items {
			rows = append(rows, columns(item))
		}
		if noHeader {
			headers = nil
		}
		for i := range rows {
			for j := range rows[i] {
				rows[i][j] = clean(rows[i][j])
			}
		}
		return output.Table(cmd.OutOrStdout(), headers, rows)
	}
}

func renderRBACObject[T any](cmd *cobra.Command, value T, format string, fields [][2]string) error {
	if format == output.FormatJSON {
		return writeJSON(cmd.OutOrStdout(), value)
	}
	for _, field := range fields {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", field[0], clean(field[1])); err != nil {
			return err
		}
	}
	return nil
}

func rbacTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

func sortOrganizations(items []client.Organization) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}

func sortTeams(items []client.Team) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}

func sortRoles(items []client.Role) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}

func sortMeasurementPermissions(items []client.MeasurementPermission) {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
}

func newRBACOrganizationsCmd() *cobra.Command {
	c := &cobra.Command{Use: "organizations", Short: "Manage RBAC organizations"}
	c.AddCommand(
		newRBACOrganizationListCmd(),
		newRBACOrganizationShowCmd(),
		newRBACOrganizationCreateCmd(),
		newRBACOrganizationUpdateCmd(),
		newRBACOrganizationDeleteCmd(),
	)
	return c
}

func newRBACOrganizationListCmd() *cobra.Command {
	var f connFlags
	var format string
	var noHeader bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List organizations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", format)
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			items, err := cli.ListOrganizations(ctx)
			if err != nil {
				return err
			}
			sortOrganizations(items)
			return renderRBACList(cmd, items, "organizations", "organizations", format, noHeader,
				[]string{"id", "name", "description", "enabled", "created_at", "updated_at"},
				func(v client.Organization) []string {
					return []string{strconv.FormatInt(v.ID, 10), v.Name, v.Description, strconv.FormatBool(v.Enabled), rbacTime(v.CreatedAt), rbacTime(v.UpdatedAt)}
				})
		},
	}
	f.add(c)
	addRBACListOutputFlags(c, &format, &noHeader)
	return c
}

func newRBACOrganizationShowCmd() *cobra.Command {
	var f connFlags
	var format string
	c := &cobra.Command{
		Use:   "show <organization-id>",
		Short: "Show one organization and its teams",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			id, err := parseRBACID(args[0], "organization")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.GetOrganization(ctx, id)
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"name", item.Name}, {"description", item.Description},
				{"enabled", strconv.FormatBool(item.Enabled)}, {"created_at", rbacTime(item.CreatedAt)},
				{"updated_at", rbacTime(item.UpdatedAt)}, {"teams", strconv.Itoa(len(item.Teams))},
			})
		},
	}
	f.add(c)
	addRBACObjectOutputFlag(c, &format)
	return c
}

func newRBACOrganizationCreateCmd() *cobra.Command {
	var f connFlags
	var name, description, format string
	c := &cobra.Command{
		Use:   "create",
		Short: "Create an organization",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.CreateOrganization(ctx, client.CreateOrganizationOptions{Name: name, Description: description})
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"name", item.Name}, {"description", item.Description},
				{"enabled", strconv.FormatBool(item.Enabled)}, {"created_at", rbacTime(item.CreatedAt)},
			})
		},
	}
	f.add(c)
	c.Flags().StringVar(&name, "name", "", "organization name")
	c.Flags().StringVar(&description, "description", "", "organization description")
	c.Flags().StringVarP(&format, "output", "o", output.FormatTable, "output format: table|json")
	_ = c.MarkFlagRequired("name")
	return c
}

func newRBACOrganizationUpdateCmd() *cobra.Command {
	var f connFlags
	var name, description, format string
	var enabled bool
	c := &cobra.Command{
		Use:   "update <organization-id>",
		Short: "Update an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			id, err := parseRBACID(args[0], "organization")
			if err != nil {
				return err
			}
			var opts client.UpdateOrganizationOptions
			if cmd.Flags().Changed("name") {
				opts.Name = &name
			}
			if cmd.Flags().Changed("description") {
				opts.Description = &description
			}
			if cmd.Flags().Changed("enabled") {
				opts.Enabled = &enabled
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.UpdateOrganization(ctx, id, opts); err != nil {
				return err
			}
			return renderRBACObject(cmd, map[string]any{"id": id, "updated": true}, format, [][2]string{
				{"id", strconv.FormatInt(id, 10)}, {"updated", "true"},
			})
		},
	}
	f.add(c)
	c.Flags().StringVar(&name, "name", "", "new organization name")
	c.Flags().StringVar(&description, "description", "", "new organization description (empty clears it)")
	c.Flags().BoolVar(&enabled, "enabled", false, "enable or disable the organization")
	addRBACObjectOutputFlag(c, &format)
	return c
}

func newRBACOrganizationDeleteCmd() *cobra.Command {
	var f connFlags
	var yes bool
	c := &cobra.Command{
		Use:   "delete <organization-id>",
		Short: "Delete an organization and its descendants",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRBACID(args[0], "organization")
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete organization id %d and all its teams, roles, and memberships?", id), yes); err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.DeleteOrganization(ctx, id); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted organization id %d\n", id)
			return err
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newRBACTeamsCmd() *cobra.Command {
	c := &cobra.Command{Use: "teams", Short: "Manage organization teams"}
	c.AddCommand(newRBACTeamListCmd(), newRBACTeamShowCmd(), newRBACTeamCreateCmd(), newRBACTeamUpdateCmd(), newRBACTeamDeleteCmd())
	return c
}

func newRBACTeamListCmd() *cobra.Command {
	var f connFlags
	var format string
	var noHeader bool
	c := &cobra.Command{
		Use:   "list <organization-id>",
		Short: "List an organization's teams",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", format)
			}
			id, err := parseRBACID(args[0], "organization")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			items, err := cli.ListTeams(ctx, id)
			if err != nil {
				return err
			}
			sortTeams(items)
			return renderRBACList(cmd, items, "teams", "teams", format, noHeader,
				[]string{"id", "organization_id", "name", "description", "enabled", "created_at", "updated_at"},
				func(v client.Team) []string {
					return []string{strconv.FormatInt(v.ID, 10), strconv.FormatInt(v.OrganizationID, 10), v.Name, v.Description, strconv.FormatBool(v.Enabled), rbacTime(v.CreatedAt), rbacTime(v.UpdatedAt)}
				})
		},
	}
	f.add(c)
	addRBACListOutputFlags(c, &format, &noHeader)
	return c
}

func newRBACTeamShowCmd() *cobra.Command {
	var f connFlags
	var format string
	c := &cobra.Command{
		Use:   "show <team-id>",
		Short: "Show one team and its roles",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			id, err := parseRBACID(args[0], "team")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.GetTeam(ctx, id)
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"organization_id", strconv.FormatInt(item.OrganizationID, 10)},
				{"name", item.Name}, {"description", item.Description}, {"enabled", strconv.FormatBool(item.Enabled)},
				{"created_at", rbacTime(item.CreatedAt)}, {"updated_at", rbacTime(item.UpdatedAt)},
				{"roles", strconv.Itoa(len(item.Roles))},
			})
		},
	}
	f.add(c)
	addRBACObjectOutputFlag(c, &format)
	return c
}

func newRBACTeamCreateCmd() *cobra.Command {
	var f connFlags
	var name, description, format string
	c := &cobra.Command{
		Use:   "create <organization-id>",
		Short: "Create a team in an organization",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			orgID, err := parseRBACID(args[0], "organization")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.CreateTeam(ctx, orgID, client.CreateTeamOptions{Name: name, Description: description})
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"organization_id", strconv.FormatInt(item.OrganizationID, 10)},
				{"name", item.Name}, {"description", item.Description}, {"enabled", strconv.FormatBool(item.Enabled)},
				{"created_at", rbacTime(item.CreatedAt)},
			})
		},
	}
	f.add(c)
	c.Flags().StringVar(&name, "name", "", "team name")
	c.Flags().StringVar(&description, "description", "", "team description")
	addRBACObjectOutputFlag(c, &format)
	_ = c.MarkFlagRequired("name")
	return c
}

func newRBACTeamUpdateCmd() *cobra.Command {
	var f connFlags
	var name, description, format string
	var enabled bool
	c := &cobra.Command{
		Use:   "update <team-id>",
		Short: "Update a team",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			id, err := parseRBACID(args[0], "team")
			if err != nil {
				return err
			}
			var opts client.UpdateTeamOptions
			if cmd.Flags().Changed("name") {
				opts.Name = &name
			}
			if cmd.Flags().Changed("description") {
				opts.Description = &description
			}
			if cmd.Flags().Changed("enabled") {
				opts.Enabled = &enabled
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.UpdateTeam(ctx, id, opts); err != nil {
				return err
			}
			return renderRBACObject(cmd, map[string]any{"id": id, "updated": true}, format, [][2]string{
				{"id", strconv.FormatInt(id, 10)}, {"updated", "true"},
			})
		},
	}
	f.add(c)
	c.Flags().StringVar(&name, "name", "", "new team name")
	c.Flags().StringVar(&description, "description", "", "new team description (empty clears it)")
	c.Flags().BoolVar(&enabled, "enabled", false, "enable or disable the team")
	addRBACObjectOutputFlag(c, &format)
	return c
}

func newRBACTeamDeleteCmd() *cobra.Command {
	var f connFlags
	var yes bool
	c := &cobra.Command{
		Use:   "delete <team-id>",
		Short: "Delete a team and its roles and memberships",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRBACID(args[0], "team")
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete team id %d and all its roles and token memberships?", id), yes); err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.DeleteTeam(ctx, id); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted team id %d\n", id)
			return err
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newRBACRolesCmd() *cobra.Command {
	c := &cobra.Command{Use: "roles", Short: "Manage team roles"}
	c.AddCommand(newRBACRoleListCmd(), newRBACRoleShowCmd(), newRBACRoleCreateCmd(), newRBACRoleUpdateCmd(), newRBACRoleDeleteCmd())
	return c
}

func newRBACRoleListCmd() *cobra.Command {
	var f connFlags
	var format string
	var noHeader bool
	c := &cobra.Command{
		Use:   "list <team-id>",
		Short: "List a team's roles",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", format)
			}
			id, err := parseRBACID(args[0], "team")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			items, err := cli.ListRoles(ctx, id)
			if err != nil {
				return err
			}
			sortRoles(items)
			return renderRBACList(cmd, items, "roles", "roles", format, noHeader,
				[]string{"id", "team_id", "database_pattern", "permissions", "created_at"},
				func(v client.Role) []string {
					return []string{strconv.FormatInt(v.ID, 10), strconv.FormatInt(v.TeamID, 10), v.DatabasePattern, strings.Join(v.Permissions, ","), rbacTime(v.CreatedAt)}
				})
		},
	}
	f.add(c)
	addRBACListOutputFlags(c, &format, &noHeader)
	return c
}

func newRBACRoleShowCmd() *cobra.Command {
	var f connFlags
	var format string
	c := &cobra.Command{
		Use:   "show <role-id>",
		Short: "Show a role and its measurement permissions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			id, err := parseRBACID(args[0], "role")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.GetRole(ctx, id)
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"team_id", strconv.FormatInt(item.TeamID, 10)},
				{"database_pattern", item.DatabasePattern}, {"permissions", strings.Join(item.Permissions, ",")},
				{"created_at", rbacTime(item.CreatedAt)}, {"measurement_permissions", strconv.Itoa(len(item.MeasurementPermissions))},
			})
		},
	}
	f.add(c)
	addRBACObjectOutputFlag(c, &format)
	return c
}

func newRBACRoleCreateCmd() *cobra.Command {
	var f connFlags
	var databasePattern, format string
	var permissions []string
	c := &cobra.Command{
		Use:   "create <team-id>",
		Short: "Create a role for a team",
		Long:  "Create a team role for a database pattern. Repeat --permission for each grant; valid values are read, write, delete, and admin.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			teamID, err := parseRBACID(args[0], "team")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.CreateRole(ctx, teamID, client.CreateRoleOptions{DatabasePattern: databasePattern, Permissions: normalisePermissions(permissions)})
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"team_id", strconv.FormatInt(item.TeamID, 10)},
				{"database_pattern", item.DatabasePattern}, {"permissions", strings.Join(item.Permissions, ",")},
				{"created_at", rbacTime(item.CreatedAt)},
			})
		},
	}
	f.add(c)
	c.Flags().StringVar(&databasePattern, "database-pattern", "", "database name or pattern")
	c.Flags().StringArrayVar(&permissions, "permission", nil, "permission to grant (repeatable: read, write, delete, admin)")
	c.Flags().StringVarP(&format, "output", "o", output.FormatTable, "output format: table|json")
	_ = c.MarkFlagRequired("database-pattern")
	_ = c.MarkFlagRequired("permission")
	return c
}

func newRBACRoleUpdateCmd() *cobra.Command {
	var f connFlags
	var databasePattern, format string
	var permissions []string
	c := &cobra.Command{
		Use:   "update <role-id>",
		Short: "Update a role's database pattern or permissions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			id, err := parseRBACID(args[0], "role")
			if err != nil {
				return err
			}
			var opts client.UpdateRoleOptions
			if cmd.Flags().Changed("database-pattern") {
				opts.DatabasePattern = &databasePattern
			}
			if cmd.Flags().Changed("permission") {
				p := normalisePermissions(permissions)
				opts.Permissions = &p
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.UpdateRole(ctx, id, opts); err != nil {
				return err
			}
			return renderRBACObject(cmd, map[string]any{"id": id, "updated": true}, format, [][2]string{
				{"id", strconv.FormatInt(id, 10)}, {"updated", "true"},
			})
		},
	}
	f.add(c)
	c.Flags().StringVar(&databasePattern, "database-pattern", "", "new database name or pattern")
	c.Flags().StringArrayVar(&permissions, "permission", nil, "replacement permission list (repeatable)")
	addRBACObjectOutputFlag(c, &format)
	return c
}

func newRBACRoleDeleteCmd() *cobra.Command {
	var f connFlags
	var yes bool
	c := &cobra.Command{
		Use:   "delete <role-id>",
		Short: "Delete a role and its measurement permissions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRBACID(args[0], "role")
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete role id %d and all its measurement permissions?", id), yes); err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.DeleteRole(ctx, id); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted role id %d\n", id)
			return err
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newRBACMeasurementsCmd() *cobra.Command {
	c := &cobra.Command{Use: "measurements", Short: "Manage role measurement permissions"}
	c.AddCommand(newRBACMeasurementListCmd(), newRBACMeasurementCreateCmd(), newRBACMeasurementDeleteCmd())
	return c
}

func newRBACMeasurementListCmd() *cobra.Command {
	var f connFlags
	var format string
	var noHeader bool
	c := &cobra.Command{
		Use:   "list <role-id>",
		Short: "List a role's measurement permissions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", format)
			}
			id, err := parseRBACID(args[0], "role")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			items, err := cli.ListMeasurementPermissions(ctx, id)
			if err != nil {
				return err
			}
			sortMeasurementPermissions(items)
			return renderRBACList(cmd, items, "measurement permissions", "measurement_permissions", format, noHeader,
				[]string{"id", "role_id", "measurement_pattern", "permissions", "created_at"},
				func(v client.MeasurementPermission) []string {
					return []string{strconv.FormatInt(v.ID, 10), strconv.FormatInt(v.RoleID, 10), v.MeasurementPattern, strings.Join(v.Permissions, ","), rbacTime(v.CreatedAt)}
				})
		},
	}
	f.add(c)
	addRBACListOutputFlags(c, &format, &noHeader)
	return c
}

func newRBACMeasurementCreateCmd() *cobra.Command {
	var f connFlags
	var pattern, format string
	var permissions []string
	c := &cobra.Command{
		Use:   "create <role-id>",
		Short: "Add measurement permissions to a role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			roleID, err := parseRBACID(args[0], "role")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.CreateMeasurementPermission(ctx, roleID, client.CreateMeasurementPermissionOptions{
				MeasurementPattern: pattern,
				Permissions:        normalisePermissions(permissions),
			})
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"role_id", strconv.FormatInt(item.RoleID, 10)},
				{"measurement_pattern", item.MeasurementPattern}, {"permissions", strings.Join(item.Permissions, ",")},
				{"created_at", rbacTime(item.CreatedAt)},
			})
		},
	}
	f.add(c)
	c.Flags().StringVar(&pattern, "measurement-pattern", "", "measurement name or pattern")
	c.Flags().StringArrayVar(&permissions, "permission", nil, "permission to grant (repeatable)")
	c.Flags().StringVarP(&format, "output", "o", output.FormatTable, "output format: table|json")
	_ = c.MarkFlagRequired("measurement-pattern")
	_ = c.MarkFlagRequired("permission")
	return c
}

func newRBACMeasurementDeleteCmd() *cobra.Command {
	var f connFlags
	var yes bool
	c := &cobra.Command{
		Use:   "delete <permission-id>",
		Short: "Remove a role's measurement permission",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseRBACID(args[0], "measurement permission")
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Delete measurement permission id %d?", id), yes); err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.DeleteMeasurementPermission(ctx, id); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted measurement permission id %d\n", id)
			return err
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}

func newRBACTokensCmd() *cobra.Command {
	c := &cobra.Command{Use: "tokens", Short: "Manage token RBAC memberships"}
	c.AddCommand(newRBACTokenTeamsCmd())
	return c
}

func newRBACTokenTeamsCmd() *cobra.Command {
	c := &cobra.Command{Use: "teams", Short: "Manage a token's team memberships"}
	c.AddCommand(newRBACTokenTeamsListCmd(), newRBACTokenTeamsAddCmd(), newRBACTokenTeamsRemoveCmd())
	return c
}

func newRBACTokenTeamsListCmd() *cobra.Command {
	var f connFlags
	var format string
	var noHeader bool
	c := &cobra.Command{
		Use:   "list <token-id|name>",
		Short: "List a token's teams",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validListFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json, csv)", format)
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			token, err := resolveRBACToken(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			cli, ctx, cancel, err = rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			items, err := cli.ListTokenTeams(ctx, token.ID)
			if err != nil {
				return err
			}
			sortTeams(items)
			return renderRBACList(cmd, items, "teams", "teams", format, noHeader,
				[]string{"id", "organization_id", "name", "description", "enabled"},
				func(v client.Team) []string {
					return []string{strconv.FormatInt(v.ID, 10), strconv.FormatInt(v.OrganizationID, 10), v.Name, v.Description, strconv.FormatBool(v.Enabled)}
				})
		},
	}
	f.add(c)
	addRBACListOutputFlags(c, &format, &noHeader)
	return c
}

func newRBACTokenTeamsAddCmd() *cobra.Command {
	var f connFlags
	var format string
	c := &cobra.Command{
		Use:   "add <token-id|name> <team-id>",
		Short: "Add a token to a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !validObjectFormat(format) {
				return fmt.Errorf("invalid --output %q (valid: table, json)", format)
			}
			teamID, err := parseRBACID(args[1], "team")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			token, err := resolveRBACToken(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			cli, ctx, cancel, err = rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			item, err := cli.AddTokenToTeam(ctx, token.ID, teamID)
			if err != nil {
				return err
			}
			return renderRBACObject(cmd, item, format, [][2]string{
				{"id", strconv.FormatInt(item.ID, 10)}, {"token_id", strconv.FormatInt(item.TokenID, 10)},
				{"team_id", strconv.FormatInt(item.TeamID, 10)}, {"created_at", rbacTime(item.CreatedAt)},
			})
		},
	}
	f.add(c)
	addRBACObjectOutputFlag(c, &format)
	return c
}

func newRBACTokenTeamsRemoveCmd() *cobra.Command {
	var f connFlags
	var yes bool
	c := &cobra.Command{
		Use:   "remove <token-id|name> <team-id>",
		Short: "Remove a token from a team",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			teamID, err := parseRBACID(args[1], "team")
			if err != nil {
				return err
			}
			cli, ctx, cancel, err := rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			token, err := resolveRBACToken(ctx, cli, args[0])
			cancel()
			if err != nil {
				return err
			}
			if err := confirmOrAbort(cmd, fmt.Sprintf("Remove token %q (id %d) from team id %d?", clean(token.Name), token.ID, teamID), yes); err != nil {
				return err
			}
			cli, ctx, cancel, err = rbacClientContext(cmd, &f)
			if err != nil {
				return err
			}
			defer cancel()
			if err := cli.RemoveTokenFromTeam(ctx, token.ID, teamID); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed token %q (id %d) from team id %d\n", clean(token.Name), token.ID, teamID)
			return err
		},
	}
	f.add(c)
	c.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return c
}
