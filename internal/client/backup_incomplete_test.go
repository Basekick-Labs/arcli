package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Arc 26.09.3 reports a backup's incompleteness on every endpoint (arc#977);
// the client must decode each count and sample under the server's keys and
// read zero where an older server omits them.
func TestBackup_IncompleteShapes(t *testing.T) {
	long := "mydb/cpu/2026/09/17/00/" + strings.Repeat("x", 960) + ".parquet"
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/backup/":
			_, _ = w.Write([]byte(`{"backups":[{"backup_id":"backup-20260917-000000-aaaaaaaa","created_at":"2026-09-17T00:00:00Z","backup_type":"full","total_files":21,"total_size_bytes":2086,"database_count":1,"skipped_files":3,"skipped_metadata_files":1,"unaddressable_files":2},{"backup_id":"backup-20260916-000000-bbbbbbbb","created_at":"2026-09-16T00:00:00Z","backup_type":"full","total_files":20,"total_size_bytes":2000,"database_count":1}],"count":2}`))
		case "/api/v1/backup/backup-20260917-000000-aaaaaaaa":
			_, _ = w.Write([]byte(`{"version":"26.09.3","backup_id":"backup-20260917-000000-aaaaaaaa","created_at":"2026-09-17T00:00:00Z","backup_type":"full","databases":[{"name":"mydb","measurements":[{"name":"cpu","file_count":21,"size_bytes":2086}],"file_count":21,"size_bytes":2086}],"total_files":21,"total_size_bytes":2086,"skipped_files":3,"skipped_metadata_files":1,"skipped_sample":["mydb/cpu/2026/09/17/00/a.parquet","mydb/cpu/2026/09/17/00/b.parquet","` + long + `","mydb/cpu/metadata/00003-5f2c.metadata.json"],"skipped_overlong_keys":1,"unaddressable_files":2,"unaddressable_sample":["mydb/cpu/2026/09/17/00/.hidden.parquet","mydb/cpu/2026/09/17/00/bad key.parquet"],"iceberg_warehouse":{"path":"/srv/wh","file_count":4,"size_bytes":100,"skipped_files":1},"has_metadata":true,"has_config":false}`))
		case "/api/v1/backup/status":
			_, _ = w.Write([]byte(`{"operation":"restore","backup_id":"backup-20260917-000000-aaaaaaaa","status":"failed","total_files":17,"processed_files":16,"skipped_files":1,"unaddressable_files":1,"unaddressable_sample":["backup-20260917-000000-aaaaaaaa/data/mydb/cpu/2026/09/17/00/.hidden.parquet"],"skipped_sample":["backup-20260917-000000-aaaaaaaa/data/mydb/cpu/2026/09/17/00/c.parquet"],"missing_files":1,"backup_skipped_files":3,"backup_unaddressable_files":2,"iceberg_warehouse_files_skipped":4,"total_bytes":2086,"processed_bytes":1600,"started_at":"2026-09-17T01:00:00Z","completed_at":"2026-09-17T01:00:02Z","error":"restore incomplete: 1 objects could not be read from backup storage (skipped_sample)"}`))
		}
	})
	ctx := context.Background()
	list, _, err := cli.ListBackups(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	if list[0].SkippedFiles != 3 || list[0].SkippedMetadataFiles != 1 || list[0].UnaddressableFiles != 2 {
		t.Errorf("incomplete entry = %+v", list[0])
	}
	if list[1].SkippedFiles != 0 || list[1].SkippedMetadataFiles != 0 || list[1].UnaddressableFiles != 0 {
		t.Errorf("entry without the keys must read zero: %+v", list[1])
	}
	m, err := cli.GetBackup(ctx, "backup-20260917-000000-aaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if m.SkippedFiles != 3 || m.SkippedMetadataFiles != 1 || m.SkippedOverlongKeys != 1 || m.UnaddressableFiles != 2 {
		t.Errorf("manifest counts = %+v", m)
	}
	if len(m.SkippedSample) != 4 || m.SkippedSample[2] != long || len(m.UnaddressableSample) != 2 {
		t.Errorf("manifest samples = %v / %v", m.SkippedSample, m.UnaddressableSample)
	}
	if m.IcebergWarehouse == nil || m.IcebergWarehouse.SkippedFiles != 1 || m.IcebergWarehouse.Path != "/srv/wh" {
		t.Errorf("iceberg warehouse = %+v", m.IcebergWarehouse)
	}
	st, err := cli.BackupStatus(ctx)
	if err != nil || st.Progress == nil {
		t.Fatalf("status=%+v err=%v", st, err)
	}
	p := st.Progress
	if len(p.SkippedSample) != 1 || len(p.UnaddressableSample) != 1 || p.UnaddressableFiles != 1 ||
		p.BackupSkippedFiles != 3 || p.BackupUnaddressableFiles != 2 || p.IcebergWarehouseFilesSkipped != 4 {
		t.Errorf("restore progress = %+v", p)
	}
}
