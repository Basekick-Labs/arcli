package commands

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"github.com/basekick-labs/arcli/internal/client"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const mqttConfigWithCredentials = `{"name":"ingest","broker":"mqtts://broker.example:8883","client_id":"arcli-test","topics":["sensors/#"],"qos":1,"database":"metrics","username":"broker-user","password":"broker-secret"}`

func runMQTTCommand(t *testing.T, input string, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("ARCLI_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	root := NewRoot(BuildInfo{Version: "test"})
	root.SetIn(strings.NewReader(input))
	return execCmd(t, root, args...)
}

func TestMQTTSubscriptionCreateReadsCredentialsFromStdinAndRedactsOutput(t *testing.T) {
	var request map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/mqtt/subscriptions/" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"subscription":{"id":"s1","name":"ingest","broker":"mqtts://broker-user:broker-secret@broker.example:8883","username":"broker-user","has_password":true,"status":"stopped"}}`))
	}))
	defer srv.Close()

	stdout, _, err := runMQTTCommand(t, mqttConfigWithCredentials,
		"mqtt", "subscription", "create", "--config-stdin", "--output", "json",
		"--endpoint", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if string(request["password"]) != `"broker-secret"` || string(request["username"]) != `"broker-user"` {
		t.Fatalf("server did not receive config body: %s", request)
	}
	for _, secret := range []string{"broker-secret", "broker-user", `"username"`, `"password"`} {
		if strings.Contains(stdout, secret) {
			t.Errorf("stdout exposed %q: %s", secret, stdout)
		}
	}
	if !strings.Contains(stdout, `"has_password": true`) || !strings.Contains(stdout, "broker.example") {
		t.Errorf("JSON output should retain safe subscription fields: %s", stdout)
	}
}

func TestMQTTSubscriptionConfigFileMustBePrivateWhenItContainsCredentials(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"subscription":{"id":"s1","name":"ingest","status":"stopped"}}`))
	}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "mqtt.json")
	if err := os.WriteFile(path, []byte(mqttConfigWithCredentials), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"mqtt", "subscription", "create", "--config-file", path, "--endpoint", srv.URL, "--token", "tok"}
	_, _, err := runMQTTCommand(t, "", args...)
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "use --config-stdin") || strings.Contains(err.Error(), "chmod") || calls != 0 {
			t.Fatalf("Windows file guidance err=%v requests=%d", err, calls)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "chmod 600") || calls != 0 {
		t.Fatalf("insecure file err=%v requests=%d", err, calls)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = runMQTTCommand(t, "", args...)
	if err != nil || calls != 1 {
		t.Fatalf("private file err=%v requests=%d", err, calls)
	}
	uriCredentialConfig := `{"name":"ingest","broker":"mqtts://broker-user:broker-secret@broker.example:8883","topics":["sensors/#"],"database":"metrics"}`
	if err := os.WriteFile(path, []byte(uriCredentialConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err = runMQTTCommand(t, "", args...)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") || calls != 1 {
		t.Fatalf("broker URL credentials in insecure file err=%v requests=%d", err, calls)
	}
}

func TestMQTTSubscriptionListFormatsRedactBrokerCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mqtt/subscriptions/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"success":true,"subscriptions":[{"id":"s1","name":"ingest","broker":"mqtts://broker-user:broker-secret@broker.example:8883","username":"broker-user","has_password":true,"status":"running","topics":["sensors/#"]}],"count":1}`))
	}))
	defer srv.Close()

	for _, format := range []string{"table", "json", "csv"} {
		stdout, _, err := runMQTTCommand(t, "", "mqtt", "subscription", "list", "--output", format, "--endpoint", srv.URL, "--token", "tok")
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		for _, secret := range []string{"broker-user", "broker-secret", `"username"`, `"password"`} {
			if strings.Contains(stdout, secret) {
				t.Errorf("%s output exposed %q: %s", format, secret, stdout)
			}
		}
		if !strings.Contains(stdout, "broker.example") {
			t.Errorf("%s output lost broker host: %s", format, stdout)
		}
	}
}

func TestMQTTDeleteRequiresConfirmation(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.Method + " " + r.URL.Path {
		case "DELETE /api/v1/mqtt/subscriptions/s1":
			_, _ = w.Write([]byte(`{"success":true,"message":"Subscription deleted"}`))
		case "POST /api/v1/mqtt/subscriptions/s1/stop":
			_, _ = w.Write([]byte(`{"success":true,"message":"Subscription stopped"}`))
		case "POST /api/v1/mqtt/subscriptions/s1/start":
			_, _ = w.Write([]byte(`{"success":true,"message":"Subscription started"}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	args := []string{"mqtt", "subscription", "delete", "s1", "--endpoint", srv.URL, "--token", "tok"}
	_, _, err := runMQTTCommand(t, "n\n", args...)
	if err == nil || err.Error() != "aborted" || calls != 0 {
		t.Fatalf("declined delete err=%v requests=%d", err, calls)
	}
	stdout, _, err := runMQTTCommand(t, "", append(args, "--yes")...)
	if err != nil || calls != 1 || !strings.Contains(stdout, "Deleted MQTT subscription s1") {
		t.Fatalf("confirmed delete stdout=%q err=%v requests=%d", stdout, err, calls)
	}
	stopArgs := []string{"mqtt", "subscription", "stop", "s1", "--endpoint", srv.URL, "--token", "tok"}
	_, _, err = runMQTTCommand(t, "n\n", stopArgs...)
	if err == nil || err.Error() != "aborted" || calls != 1 {
		t.Fatalf("declined stop err=%v requests=%d", err, calls)
	}
	stdout, _, err = runMQTTCommand(t, "yes\n", stopArgs...)
	if err != nil || calls != 2 || !strings.Contains(stdout, "Subscription stopped") {
		t.Fatalf("confirmed stop stdout=%q err=%v requests=%d", stdout, err, calls)
	}
	stdout, _, err = runMQTTCommand(t, "", "mqtt", "subscription", "start", "s1", "--endpoint", srv.URL, "--token", "tok")
	if err != nil || calls != 3 || !strings.Contains(stdout, "Subscription started") {
		t.Fatalf("start stdout=%q err=%v requests=%d", stdout, err, calls)
	}
}

func TestMQTTCSVPreservesNonSecretValuesAndTimestamps(t *testing.T) {
	stamp, _ := time.Parse(time.RFC3339Nano, "2026-10-31T12:34:56.123456789+05:30")
	sub := client.MQTTSubscription{ID: " id\t", Name: " a,\"b\"\n\x1b[31m ", Status: "running\t", Broker: "mqtts://broker-user:broker-secret@host:8883", ClientID: " c\n", Topics: []string{"sensors/\t#", "other,topic"}, Database: " db ", HasPassword: true, CreatedAt: stamp, UpdatedAt: stamp}
	// The list (table and CSV) carries no timestamp columns; created/updated
	// are shown by `subscription show` and kept in JSON.
	for _, noHeader := range []bool{false, true} {
		var out bytes.Buffer
		if err := renderMQTTSubscriptionList(&out, []client.MQTTSubscription{sub}, noHeader, true); err != nil {
			t.Fatal(err)
		}
		rows, err := csv.NewReader(&out).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		if !noHeader {
			if !reflect.DeepEqual(rows[0], []string{"id", "name", "status", "broker", "client_id", "topics", "qos", "database", "tls", "auto_start", "password"}) {
				t.Fatalf("headers=%q", rows[0])
			}
			rows = rows[1:]
		}
		want := []string{sub.ID, sub.Name, sub.Status, "mqtts://host:8883", sub.ClientID, strings.Join(sub.Topics, ","), "0", sub.Database, "false", "false", "set"}
		if len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
			t.Fatalf("CSV=%q want=%q", rows, want)
		}
	}
	var table bytes.Buffer
	if err := renderMQTTSubscriptionList(&table, []client.MQTTSubscription{sub}, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(table.String(), "\x1b") || strings.Contains(table.String(), "broker-secret") || strings.Contains(table.String(), "2026") || strings.Contains(table.String(), "CREATED") || strings.Contains(table.String(), "UPDATED") {
		t.Fatalf("table=%q", table.String())
	}
	for _, mode := range []struct {
		name string
		to   timeOutput
		utc  string
	}{
		{"default", timeOutput{}, "2026-10-31T07:04:56Z"},
		{"rfc3339nano", timeOutput{nano: true}, "2026-10-31T07:04:56.123456789Z"},
	} {
		var detail bytes.Buffer
		writeMQTTSubscriptionInfo(&detail, &sub, mode.to)
		if !strings.Contains(detail.String(), mode.utc) || strings.Contains(detail.String(), "broker-secret") {
			t.Fatalf("%s: detail=%s", mode.name, detail.String())
		}
		if !mode.to.nano && strings.Contains(detail.String(), ".123") {
			t.Fatalf("default output kept fractional seconds:\n%s", detail.String())
		}
	}
	data, err := json.Marshal(safeMQTTSubscription(sub))
	if err != nil {
		t.Fatal(err)
	}
	var got client.MQTTSubscription
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != sub.Name || !got.CreatedAt.Equal(stamp) || got.CreatedAt.Nanosecond() != 123456789 || strings.Contains(string(data), "broker-secret") || strings.Contains(string(data), "broker-user") {
		t.Fatalf("JSON=%s", data)
	}
}

// Unset timestamps keep the "never" placeholder in the detail view; the
// list has no timestamp columns, so it neither shows "never" nor a zero time.
func TestMQTTZeroTimestamps(t *testing.T) {
	sub := client.MQTTSubscription{ID: "s1"}
	var out bytes.Buffer
	if err := renderMQTTSubscriptionList(&out, []client.MQTTSubscription{sub}, true, true); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out.String(), ",no\n") || strings.Contains(out.String(), "0001") {
		t.Fatalf("CSV=%q", out.String())
	}
	var table, detail bytes.Buffer
	if err := renderMQTTSubscriptionList(&table, []client.MQTTSubscription{sub}, false, false); err != nil {
		t.Fatal(err)
	}
	writeMQTTSubscriptionInfo(&detail, &sub, timeOutput{})
	if strings.Contains(table.String(), "never") || strings.Contains(table.String(), "0001") || strings.Count(detail.String(), "never") != 2 {
		t.Fatalf("table=%s\ndetail=%s", table.String(), detail.String())
	}
}

func TestMQTTBrokerRedactionDoesNotSanitizeNonSecrets(t *testing.T) {
	for _, broker := range []string{"mqtt://host/a%2fb", "mqtt://host/path?x=a,b", "invalid\tbroker"} {
		if got := scrubMQTTBroker(broker); got != broker {
			t.Errorf("broker=%q got=%q", broker, got)
		}
	}
	if got := scrubMQTTBroker("mqtt://user:secret@host/\n"); got != "<redacted>" {
		t.Errorf("malformed credential URL=%q", got)
	}
}

func TestMQTTStatsTimestamps(t *testing.T) {
	stamp, _ := time.Parse(time.RFC3339Nano, "2026-10-31T02:03:04.123456789-03:30")
	stats := client.MQTTSubscriptionStats{LastMessageAt: &stamp, ConnectedSince: &stamp}
	for _, mode := range []struct {
		to   timeOutput
		want string
	}{
		{timeOutput{}, "2026-10-31T05:33:04Z"},
		{timeOutput{nano: true}, "2026-10-31T05:33:04.123456789Z"},
	} {
		for _, object := range []bool{false, true} {
			var out bytes.Buffer
			if err := renderMQTTStats(&out, []client.MQTTSubscriptionStats{stats}, object, mode.to); err != nil {
				t.Fatal(err)
			}
			if strings.Count(out.String(), mode.want) != 2 {
				t.Fatalf("stats=%s", out.String())
			}
		}
	}
	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(data), "2026-10-31T02:03:04.123456789-03:30") != 2 {
		t.Fatalf("JSON=%s", data)
	}
}

func TestMQTTReviewOutputAndExitStatus(t *testing.T) {
	for _, format := range []string{"table", "json"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/health") {
					_, _ = w.Write([]byte(`{"status":"degraded","healthy":false}`))
					return
				}
				_, _ = w.Write([]byte(`{"subscription":{"id":"s1","status":"error","broker":"ws://host/mqtt?token=QSsecret123&keep=yes","tls_cert_path":"/cert.pem","tls_key_path":"/key.pem","tls_ca_path":"/ca.pem","topic_mapping":{"a":"simple","b":{"measurement":"temperature"}}}}`))
			}))
			defer srv.Close()
			out, _, err := runMQTTCommand(t, `{}`, "mqtt", "subscription", "create", "--config-stdin", "-o", format, "--endpoint", srv.URL, "--token", "tok")
			if err == nil {
				t.Fatal("error-status create must fail")
			}
			if strings.Contains(out, "QSsecret123") || !strings.Contains(out, "temperature") || !strings.Contains(out, "/cert.pem") {
				t.Fatalf("output: %s", out)
			}
			out, _, err = runMQTTCommand(t, "", "mqtt", "health", "-o", format, "--endpoint", srv.URL, "--token", "tok")
			if err == nil || !strings.Contains(out, "degraded") {
				t.Fatalf("health output=%s err=%v", out, err)
			}
		})
	}
}

func TestMQTTQueryCredentialsRequirePrivateFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses config-stdin")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"broker":"ws://host/mqtt?token=QSsecret123"}`), 0644); err != nil {
		t.Fatal(err)
	}
	_, _, err := runMQTTCommand(t, "", "mqtt", "subscription", "create", "--config-file", path)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("permissions: %v", err)
	}
}

func TestMQTTObjectMappingPassesThroughCreateAndUpdate(t *testing.T) {
	for _, action := range []string{"create", "update"} {
		t.Run(action, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				if !strings.Contains(string(req["topic_mapping"]), `"measurement":"temp"`) {
					t.Errorf("lost mapping: %s", req["topic_mapping"])
				}
				_, _ = w.Write([]byte(`{"subscription":{"id":"s1","status":"stopped","topic_mapping":{"a":{"measurement":"temp"}}}}`))
			}))
			defer srv.Close()
			args := []string{"mqtt", "subscription", action}
			if action == "update" {
				args = append(args, "s1")
			}
			args = append(args, "--config-stdin", "--endpoint", srv.URL, "--token", "tok")
			out, _, err := runMQTTCommand(t, `{"topic_mapping":{"a":{"measurement":"temp"}}}`, args...)
			if err != nil || !strings.Contains(out, `{"measurement":"temp"}`) {
				t.Fatalf("out=%s err=%v", out, err)
			}
		})
	}
}

func TestMQTTQueryCredentialsRedactedInEveryListFormat(t *testing.T) {
	for _, format := range []string{"table", "json", "csv"} {
		t.Run(format, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"subscriptions":[{"id":"s1","broker":"ws://host/mqtt?%74oken=QSsecret123&keep=yes","topic_mapping":{"a":{"measurement":"temp"}}}]}`))
			}))
			defer srv.Close()
			out, _, err := runMQTTCommand(t, "", "mqtt", "subscription", "list", "-o", format, "--endpoint", srv.URL, "--token", "tok")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out, "QSsecret123") || !strings.Contains(out, "keep=yes") {
				t.Fatalf("output: %s", out)
			}
		})
	}
}
