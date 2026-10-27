package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestMQTTClientCoversHealthStatsCRUDAndLifecycle(t *testing.T) {
	var calls []string
	var createBody map[string]json.RawMessage
	var updateBody map[string]json.RawMessage
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/mqtt/health":
			_, _ = w.Write([]byte(`{"status":"healthy","healthy":true,"subscriptions":{"total":1,"running":1,"stopped":0,"errors":0}}`))
		case "GET /api/v1/mqtt/stats":
			_, _ = w.Write([]byte(`{"success":true,"stats":{"subscriptions":[{"id":"s1","name":"ingest","status":"running","messages_received":5,"messages_failed":1,"bytes_received":1024,"reconnects":2}],"summary":{"total_subscriptions":1,"running":1,"stopped":0,"error":0,"total_messages":5,"total_failed":1,"total_bytes":1024}}}`))
		case "GET /api/v1/mqtt/subscriptions/":
			_, _ = w.Write([]byte(`{"success":true,"subscriptions":[{"id":"s1","name":"ingest","broker":"mqtts://broker.example:8883","username":"broker-user","has_password":true,"status":"running"}],"count":1}`))
		case "GET /api/v1/mqtt/subscriptions/s1":
			_, _ = w.Write([]byte(`{"success":true,"subscription":{"id":"s1","name":"ingest","broker":"mqtts://broker.example:8883","username":"broker-user","has_password":true,"status":"running"}}`))
		case "POST /api/v1/mqtt/subscriptions/":
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Errorf("decode create request: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"success":true,"subscription":{"id":"s2","name":"created","broker":"mqtts://broker.example:8883","has_password":true,"status":"stopped"}}`))
		case "PUT /api/v1/mqtt/subscriptions/s1":
			if err := json.NewDecoder(r.Body).Decode(&updateBody); err != nil {
				t.Errorf("decode update request: %v", err)
			}
			_, _ = w.Write([]byte(`{"success":true,"subscription":{"id":"s1","name":"updated","status":"stopped"}}`))
		case "DELETE /api/v1/mqtt/subscriptions/s1":
			_, _ = w.Write([]byte(`{"success":true,"message":"Subscription deleted"}`))
		case "POST /api/v1/mqtt/subscriptions/s1/start", "POST /api/v1/mqtt/subscriptions/s1/stop", "POST /api/v1/mqtt/subscriptions/s1/pause", "POST /api/v1/mqtt/subscriptions/s1/restart":
			_, _ = w.Write([]byte(`{"success":true,"message":"Subscription action completed"}`))
		case "GET /api/v1/mqtt/subscriptions/s1/stats":
			_, _ = w.Write([]byte(`{"success":true,"stats":{"id":"s1","name":"ingest","status":"running","messages_received":5,"messages_failed":1,"bytes_received":1024,"reconnects":2}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	ctx := context.Background()
	health, err := cli.MQTTHealth(ctx)
	if err != nil || health.Status != "healthy" || health.Subscriptions == nil || health.Subscriptions.Running != 1 {
		t.Fatalf("health=%+v err=%v", health, err)
	}
	stats, err := cli.MQTTStats(ctx)
	if err != nil || stats.Summary.TotalMessages != 5 || len(stats.Subscriptions) != 1 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	list, err := cli.ListMQTTSubscriptions(ctx)
	if err != nil || len(list) != 1 || list[0].HasPassword != true {
		t.Fatalf("list=%+v err=%v", list, err)
	}
	got, err := cli.GetMQTTSubscription(ctx, "s1")
	if err != nil || got.ID != "s1" {
		t.Fatalf("get=%+v err=%v", got, err)
	}
	qos := 0
	created, err := cli.CreateMQTTSubscription(ctx, MQTTSubscriptionCreateRequest{
		Name: "created", Broker: "mqtts://broker.example:8883", Topics: []string{"sensors/#"},
		QoS: &qos, Database: "metrics", Username: "broker-user", Password: "do-not-log-me",
	})
	if err != nil || created.ID != "s2" {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	if string(createBody["password"]) != `"do-not-log-me"` || string(createBody["qos"]) != "0" {
		t.Errorf("create body missing password or explicit QoS 0: %s", createBody)
	}
	newPassword := "updated-secret"
	updated, err := cli.UpdateMQTTSubscription(ctx, "s1", MQTTSubscriptionUpdateRequest{Password: &newPassword})
	if err != nil || updated.Name != "updated" || string(updateBody["password"]) != `"updated-secret"` {
		t.Fatalf("update=%+v body=%s err=%v", updated, updateBody, err)
	}
	if err := cli.DeleteMQTTSubscription(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "stop", "pause", "restart"} {
		if _, err := cli.MQTTSubscriptionAction(ctx, "s1", action); err != nil {
			t.Errorf("%s: %v", action, err)
		}
	}
	if _, err := cli.MQTTSubscriptionAction(ctx, "s1", "delete"); err == nil {
		t.Fatal("unsupported action should fail before sending a request")
	}
	if _, err := cli.GetMQTTSubscription(ctx, " "); err == nil {
		t.Fatal("blank subscription id should fail before sending a request")
	}
	subStats, err := cli.MQTTSubscriptionStats(ctx, "s1")
	if err != nil || subStats.MessagesReceived != 5 {
		t.Fatalf("subscription stats=%+v err=%v", subStats, err)
	}
	want := []string{
		"GET /api/v1/mqtt/health", "GET /api/v1/mqtt/stats", "GET /api/v1/mqtt/subscriptions/",
		"GET /api/v1/mqtt/subscriptions/s1", "POST /api/v1/mqtt/subscriptions/", "PUT /api/v1/mqtt/subscriptions/s1",
		"DELETE /api/v1/mqtt/subscriptions/s1", "POST /api/v1/mqtt/subscriptions/s1/start",
		"POST /api/v1/mqtt/subscriptions/s1/stop", "POST /api/v1/mqtt/subscriptions/s1/pause",
		"POST /api/v1/mqtt/subscriptions/s1/restart", "GET /api/v1/mqtt/subscriptions/s1/stats",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls = %v", calls)
	}
	serialized, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "broker-user") || strings.Contains(string(serialized), `"username"`) || strings.Contains(string(serialized), `"password"`) {
		t.Errorf("subscription JSON leaked credential fields: %s", serialized)
	}
}

func TestMQTTClientPreservesDisabledPermissionAndNotFoundErrors(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/mqtt/subscriptions/disabled":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"success":false,"error":"MQTT subsystem disabled"}`))
		case "/api/v1/mqtt/subscriptions/forbidden":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"success":false,"error":"Permission denied: admin required"}`))
		case "/api/v1/mqtt/subscriptions/invalid":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"success":false,"error":"invalid qos"}`))
		case "/api/v1/mqtt/subscriptions/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"error":"Subscription not found"}`))
		case "/api/v1/mqtt/subscriptions/":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Cannot GET /api/v1/mqtt/subscriptions/"}`))
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"1s"}`))
		default:
			http.NotFound(w, r)
		}
	})

	for _, tc := range []struct {
		id     string
		status int
		text   string
	}{
		{"disabled", http.StatusServiceUnavailable, "MQTT subsystem disabled"},
		{"forbidden", http.StatusForbidden, "Permission denied"},
		{"invalid", http.StatusBadRequest, "invalid qos"},
		{"missing", http.StatusNotFound, "Subscription not found"},
	} {
		_, err := cli.GetMQTTSubscription(context.Background(), tc.id)
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != tc.status || !strings.Contains(err.Error(), tc.text) {
			t.Errorf("%s error = %v", tc.id, err)
		}
	}
	_, err := cli.ListMQTTSubscriptions(context.Background())
	var disabled *FeatureDisabledError
	if !errors.As(err, &disabled) || disabled.ConfigKey != "mqtt.enabled" {
		t.Errorf("missing route error = %v", err)
	}
}
