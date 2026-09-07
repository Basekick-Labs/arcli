package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestValidateWhereAndFullTable(t *testing.T) {
	for _, ok := range []string{"host = 'a'", "time < '2026-01-01' AND x > -1", "1=1", " WHERE true "} {
		if err := ValidateWhere(ok); err != nil {
			t.Errorf("%q should pass: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "  ", "a; DROP", "x -- y", "a /* b */", "host = 'a", "(a = 1"} {
		if err := ValidateWhere(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	for _, full := range []string{"1=1", " TRUE ", "where 1", "WHERE true"} {
		if !IsFullTableWhere(full) {
			t.Errorf("%q should be full-table", full)
		}
	}
	if got := StripWhereKeyword("  WHERE host = 'a' "); got != "host = 'a'" {
		t.Errorf("strip = %q", got)
	}
	if got := StripWhereKeyword("wherever = 1"); got != "wherever = 1" {
		t.Errorf("must not strip a column named where*: %q", got)
	}
	if got := NormaliseWhere("Where host = 'a'"); got != "(host = 'a') IS TRUE" {
		t.Errorf("normalise = %q", got)
	}
	for _, notFull := range []string{"1=1 AND host='a'", "true OR false", "host = 'a'"} {
		if IsFullTableWhere(notFull) {
			t.Errorf("%q should not be full-table", notFull)
		}
	}
}

func TestDeleteRows_BodyDryRunAndSuccess(t *testing.T) {
	var got DeleteRequest
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/delete/" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"success":true,"deleted_count":2,"affected_files":1,"rewritten_files":1,"execution_time_ms":17,"dry_run":` + boolStr(got.DryRun) + `,"files_processed":["cpu_1.parquet"]}`))
	})
	res, err := cli.DeleteRows(context.Background(), DeleteRequest{Database: "d", Measurement: "cpu", Where: "host = 'a'", DryRun: true, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.DryRun || !got.Confirm || res.DeletedCount != 2 || !res.DryRun || len(res.FilesProcessed) != 1 {
		t.Errorf("got=%+v res=%+v", got, res)
	}
	for _, bad := range []DeleteRequest{
		{Measurement: "cpu", Where: "1=1"},
		{Database: "d", Where: "1=1"},
		{Database: "a/b", Measurement: "cpu", Where: "1=1"},
		{Database: "d", Measurement: "..", Where: "1=1"},
		{Database: "d", Measurement: "cpu", Where: "a; b"},
	} {
		if _, err := cli.DeleteRows(context.Background(), bad); err == nil {
			t.Errorf("%+v should be rejected client-side", bad)
		}
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestDeleteRows_ErrorShapes(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req DeleteRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Where {
		case "disabled":
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"deleted_count":0,"affected_files":0,"rewritten_files":0,"execution_time_ms":0,"dry_run":false,"files_processed":null,"error":"Delete operations are disabled. Set delete.enabled=true in arc.toml to enable."}`))
		case "abort":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"success":false,"deleted_count":3,"affected_files":4,"rewritten_files":1,"execution_time_ms":9,"dry_run":false,"files_processed":["a.parquet"],"error":"Delete aborted: manifest failure"}`))
		case "partial":
			w.WriteHeader(207)
			_, _ = w.Write([]byte(`{"success":false,"deleted_count":5,"affected_files":2,"rewritten_files":1,"execution_time_ms":9,"dry_run":false,"files_processed":["a.parquet"],"failed_files":["b.parquet"],"error":"1 of 2 files failed to process"}`))
		default:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"success":false,"files_processed":null,"error":"WHERE clause contains forbidden keyword: SELECT"}`))
		}
	})
	ctx := context.Background()
	_, err := cli.DeleteRows(ctx, DeleteRequest{Database: "d", Measurement: "m", Where: "disabled"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 403 || !strings.Contains(he.Message, "delete.enabled=true") {
		t.Errorf("err = %v", err)
	}
	res, err := cli.DeleteRows(ctx, DeleteRequest{Database: "d", Measurement: "m", Where: "partial"})
	var pe *PartialDeleteError
	if !errors.As(err, &pe) || res == nil || len(pe.Result.FailedFiles) != 1 || pe.Result.DeletedCount != 5 || pe.Status != 207 {
		t.Errorf("207: err=%v res=%+v", err, res)
	}
	if !strings.Contains(err.Error(), "5 rows deleted in 1 file(s), 1 file(s) failed") {
		t.Errorf("Error() = %q", err.Error())
	}
	// A mid-run 500 that already deleted rows is a partial error too.
	_, err = cli.DeleteRows(ctx, DeleteRequest{Database: "d", Measurement: "m", Where: "abort"})
	if !errors.As(err, &pe) || pe.Status != 500 || pe.Result.DeletedCount != 3 {
		t.Errorf("500 partial: err=%v", err)
	}
	_, err = cli.DeleteRows(ctx, DeleteRequest{Database: "d", Measurement: "m", Where: "host = (x)"})
	if !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Message, "forbidden keyword") {
		t.Errorf("err = %v", err)
	}
}

func TestBackup_IDValidationAndBusy(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":"A backup or restore operation is already in progress","status":"running","operation":"restore"}`))
	})
	ctx := context.Background()
	if err := ValidateBackupID("backup-20260907-201105-a0f5e600"); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "x", "backup-2026-01-01", "backup-20260907-201105-ZZZZZZZZ", "../etc"} {
		if err := ValidateBackupID(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if _, err := cli.GetBackup(ctx, "nope"); err == nil {
		t.Error("bad id must not reach the network")
	}
	err := cli.CreateBackup(ctx, BackupCreateOptions{})
	var be *BackupBusyError
	if !errors.As(err, &be) || be.Operation != "restore" {
		t.Errorf("err = %v", err)
	}
}

func TestBackup_ListShowStatusShapes(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		switch r.URL.Path {
		case "/api/v1/backup/":
			_, _ = w.Write([]byte(`{"backups":[{"backup_id":"backup-20260907-201105-a0f5e600","created_at":"2026-09-07T20:11:05.69275Z","backup_type":"full","total_files":2,"total_size_bytes":2086,"database_count":2}],"count":1}`))
		case "/api/v1/backup/status":
			_, _ = w.Write([]byte(`{"operation":"backup","backup_id":"backup-20260907-201105-a0f5e600","status":"completed","total_files":2,"processed_files":2,"skipped_files":0,"total_bytes":2086,"processed_bytes":2086,"started_at":"2026-09-07T14:11:05.69275-06:00","completed_at":"2026-09-07T14:11:05.697886-06:00"}`))
		case "/api/v1/backup/backup-20260907-201105-a0f5e600":
			_, _ = w.Write([]byte(`{"version":"dev","backup_id":"backup-20260907-201105-a0f5e600","created_at":"2026-09-07T20:11:05.69275Z","backup_type":"full","databases":[{"name":"smoke","measurements":[{"name":"cpu","file_count":1,"size_bytes":1064}],"file_count":1,"size_bytes":1064}],"total_files":2,"total_size_bytes":2086,"has_metadata":true,"has_config":false}`))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"Backup not found"}`))
		}
	})
	ctx := context.Background()
	list, _, err := cli.ListBackups(ctx)
	if err != nil || len(list) != 1 || list[0].TotalBytes != 2086 || list[0].CreatedAt.IsZero() {
		t.Errorf("list=%+v err=%v", list, err)
	}
	st, err := cli.BackupStatus(ctx)
	if err != nil || st.Idle || st.Progress == nil || st.Progress.Status != "completed" || st.Progress.StartedAt.UTC().Hour() != 20 {
		t.Errorf("status=%+v err=%v", st, err)
	}
	m, err := cli.GetBackup(ctx, "backup-20260907-201105-a0f5e600")
	if err != nil || len(m.Databases) != 1 || m.Databases[0].Measurements[0].Name != "cpu" || m.HasConfig {
		t.Errorf("manifest=%+v err=%v", m, err)
	}
	_, err = cli.GetBackup(ctx, "backup-00000000-000000-00000000")
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 {
		t.Errorf("err = %v", err)
	}
}

func TestBackup_StatusIdleAndRestoreBody(t *testing.T) {
	var raw map[string]any
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/backup/status":
			_, _ = w.Write([]byte(`{"status":"idle"}`))
		case r.URL.Path == "/api/v1/backup/restore":
			_ = json.NewDecoder(r.Body).Decode(&raw)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"backup_id":"backup-20260907-201105-a0f5e600","message":"Restore started","status":"running","restart_required":true,"staged":true}`))
		}
	})
	ctx := context.Background()
	st, err := cli.BackupStatus(ctx)
	if err != nil || !st.Idle || st.Progress != nil {
		t.Errorf("idle: %+v err=%v", st, err)
	}
	f := false
	res, err := cli.RestoreBackup(ctx, "backup-20260907-201105-a0f5e600", RestoreOptions{RestoreMetadata: &f, RestoreConfig: &f})
	if err != nil || !res.RestartRequired || !res.Staged {
		t.Errorf("res=%+v err=%v", res, err)
	}
	if raw["confirm"] != true || raw["restore_metadata"] != false || raw["restore_config"] != false {
		t.Errorf("body = %v", raw)
	}
	if _, has := raw["restore_data"]; has {
		t.Error("nil restore_data must be omitted")
	}
}

func TestBackup_DisabledServer(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":"Cannot ` + r.Method + ` ` + r.URL.Path + `"}`))
	})
	_, _, err := cli.ListBackups(context.Background())
	var fde *FeatureDisabledError
	if !errors.As(err, &fde) || fde.ConfigKey != "backup.enabled" {
		t.Errorf("err = %v", err)
	}
}
