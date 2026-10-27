package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const mqttBase = "/api/v1/mqtt"

// MQTTHealth is the response from GET /api/v1/mqtt/health.
type MQTTHealth struct {
	Status        string                   `json:"status"`
	Healthy       bool                     `json:"healthy"`
	Error         string                   `json:"error,omitempty"`
	Subscriptions *MQTTHealthSubscriptions `json:"subscriptions,omitempty"`
	Raw           json.RawMessage          `json:"-"`
}

// MQTTHealthSubscriptions contains the health endpoint's status counts.
type MQTTHealthSubscriptions struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	Stopped int `json:"stopped"`
	Errors  int `json:"errors"`
}

// MQTTStats is the aggregate response from GET /api/v1/mqtt/stats.
type MQTTStats struct {
	Subscriptions []MQTTSubscriptionStats `json:"subscriptions"`
	Summary       MQTTStatsSummary        `json:"summary"`
	Raw           json.RawMessage         `json:"-"`
}

// MQTTStatsSummary contains aggregate message and subscription counts.
type MQTTStatsSummary struct {
	TotalSubscriptions int   `json:"total_subscriptions"`
	Running            int   `json:"running"`
	Stopped            int   `json:"stopped"`
	Error              int   `json:"error"`
	TotalMessages      int64 `json:"total_messages"`
	TotalFailed        int64 `json:"total_failed"`
	TotalBytes         int64 `json:"total_bytes"`
}

// MQTTSubscriptionStats contains runtime counters for one subscription.
type MQTTSubscriptionStats struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Status           string     `json:"status"`
	MessagesReceived int64      `json:"messages_received"`
	MessagesFailed   int64      `json:"messages_failed"`
	BytesReceived    int64      `json:"bytes_received"`
	LastMessageAt    *time.Time `json:"last_message_at,omitempty"`
	ConnectedSince   *time.Time `json:"connected_since,omitempty"`
	Reconnects       int64      `json:"reconnects"`
}

// MQTTSubscription is a safe representation of an Arc MQTT subscription.
// Username and password are intentionally never decoded or serialised by
// arcli; the server returns only has_password for the stored password.
type MQTTSubscription struct {
	ID                    string            `json:"id"`
	Name                  string            `json:"name"`
	Broker                string            `json:"broker"`
	ClientID              string            `json:"client_id"`
	Topics                []string          `json:"topics"`
	QoS                   int               `json:"qos"`
	Database              string            `json:"database"`
	Username              string            `json:"-"`
	HasPassword           bool              `json:"has_password"`
	TLSEnabled            bool              `json:"tls_enabled"`
	TLSCertPath           string            `json:"tls_cert_path,omitempty"`
	TLSKeyPath            string            `json:"tls_key_path,omitempty"`
	TLSCAPath             string            `json:"tls_ca_path,omitempty"`
	TLSInsecureSkipVerify bool              `json:"tls_insecure_skip_verify"`
	AutoStart             bool              `json:"auto_start"`
	Status                string            `json:"status"`
	ErrorMessage          string            `json:"error_message,omitempty"`
	TopicMapping          map[string]string `json:"topic_mapping,omitempty"`
	KeepAliveSeconds      int               `json:"keep_alive_seconds"`
	ConnectTimeoutSeconds int               `json:"connect_timeout_seconds"`
	ReconnectMaxSeconds   int               `json:"reconnect_max_seconds"`
	CleanSession          bool              `json:"clean_session"`
	CreatedAt             time.Time         `json:"created_at"`
	UpdatedAt             time.Time         `json:"updated_at"`
}

// MQTTSubscriptionCreateRequest is sent to POST /subscriptions/.
type MQTTSubscriptionCreateRequest struct {
	Name                  string            `json:"name"`
	Broker                string            `json:"broker"`
	ClientID              string            `json:"client_id,omitempty"`
	Topics                []string          `json:"topics"`
	QoS                   *int              `json:"qos,omitempty"`
	Database              string            `json:"database"`
	Username              string            `json:"username,omitempty"`
	Password              string            `json:"password,omitempty"`
	TLSEnabled            bool              `json:"tls_enabled"`
	TLSCertPath           string            `json:"tls_cert_path,omitempty"`
	TLSKeyPath            string            `json:"tls_key_path,omitempty"`
	TLSCAPath             string            `json:"tls_ca_path,omitempty"`
	TLSInsecureSkipVerify bool              `json:"tls_insecure_skip_verify"`
	AutoStart             bool              `json:"auto_start"`
	TopicMapping          map[string]string `json:"topic_mapping,omitempty"`
	KeepAliveSeconds      int               `json:"keep_alive_seconds,omitempty"`
	ConnectTimeoutSeconds int               `json:"connect_timeout_seconds,omitempty"`
	ReconnectMaxSeconds   int               `json:"reconnect_max_seconds,omitempty"`
	CleanSession          bool              `json:"clean_session"`
}

// MQTTSubscriptionUpdateRequest is sent to PUT /subscriptions/:id.
type MQTTSubscriptionUpdateRequest struct {
	Name                  *string            `json:"name,omitempty"`
	Broker                *string            `json:"broker,omitempty"`
	ClientID              *string            `json:"client_id,omitempty"`
	Topics                []string           `json:"topics,omitempty"`
	QoS                   *int               `json:"qos,omitempty"`
	Database              *string            `json:"database,omitempty"`
	Username              *string            `json:"username,omitempty"`
	Password              *string            `json:"password,omitempty"`
	TLSEnabled            *bool              `json:"tls_enabled,omitempty"`
	TLSCertPath           *string            `json:"tls_cert_path,omitempty"`
	TLSKeyPath            *string            `json:"tls_key_path,omitempty"`
	TLSCAPath             *string            `json:"tls_ca_path,omitempty"`
	TLSInsecureSkipVerify *bool              `json:"tls_insecure_skip_verify,omitempty"`
	AutoStart             *bool              `json:"auto_start,omitempty"`
	TopicMapping          *map[string]string `json:"topic_mapping,omitempty"`
	KeepAliveSeconds      *int               `json:"keep_alive_seconds,omitempty"`
	ConnectTimeoutSeconds *int               `json:"connect_timeout_seconds,omitempty"`
	ReconnectMaxSeconds   *int               `json:"reconnect_max_seconds,omitempty"`
	CleanSession          *bool              `json:"clean_session,omitempty"`
}

func (c *Client) mqttJSON(ctx context.Context, method, path string, reqBody any, limit int64, out any) ([]byte, error) {
	return c.featureJSON(ctx, "MQTT", "mqtt.enabled", method, path, reqBody, limit, out)
}

// MQTTHealth calls GET /api/v1/mqtt/health.
func (c *Client) MQTTHealth(ctx context.Context) (*MQTTHealth, error) {
	var out MQTTHealth
	body, err := c.mqttJSON(ctx, http.MethodGet, mqttBase+"/health", nil, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	out.Raw = body
	return &out, nil
}

// MQTTStats calls GET /api/v1/mqtt/stats.
func (c *Client) MQTTStats(ctx context.Context) (*MQTTStats, error) {
	var envelope struct {
		Stats MQTTStats `json:"stats"`
	}
	body, err := c.mqttJSON(ctx, http.MethodGet, mqttBase+"/stats", nil, 16<<20, &envelope)
	if err != nil {
		return nil, err
	}
	if envelope.Stats.Subscriptions == nil {
		envelope.Stats.Subscriptions = []MQTTSubscriptionStats{}
	}
	envelope.Stats.Raw = body
	return &envelope.Stats, nil
}

// ListMQTTSubscriptions calls GET /api/v1/mqtt/subscriptions/.
func (c *Client) ListMQTTSubscriptions(ctx context.Context) ([]MQTTSubscription, error) {
	var out struct {
		Subscriptions []MQTTSubscription `json:"subscriptions"`
	}
	_, err := c.mqttJSON(ctx, http.MethodGet, mqttBase+"/subscriptions/", nil, 16<<20, &out)
	if err != nil {
		return nil, err
	}
	if out.Subscriptions == nil {
		out.Subscriptions = []MQTTSubscription{}
	}
	return out.Subscriptions, nil
}

// GetMQTTSubscription calls GET /api/v1/mqtt/subscriptions/:id.
func (c *Client) GetMQTTSubscription(ctx context.Context, id string) (*MQTTSubscription, error) {
	if err := validateMQTTSubscriptionID(id); err != nil {
		return nil, err
	}
	var out struct {
		Subscription MQTTSubscription `json:"subscription"`
	}
	_, err := c.mqttJSON(ctx, http.MethodGet, mqttSubscriptionPath(id), nil, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	return &out.Subscription, nil
}

// CreateMQTTSubscription calls POST /api/v1/mqtt/subscriptions/.
// Passwords are sent only in the JSON request body, never in the URL.
func (c *Client) CreateMQTTSubscription(ctx context.Context, body MQTTSubscriptionCreateRequest) (*MQTTSubscription, error) {
	var out struct {
		Subscription MQTTSubscription `json:"subscription"`
	}
	_, err := c.mqttJSON(ctx, http.MethodPost, mqttBase+"/subscriptions/", body, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	return &out.Subscription, nil
}

// UpdateMQTTSubscription calls PUT /api/v1/mqtt/subscriptions/:id.
func (c *Client) UpdateMQTTSubscription(ctx context.Context, id string, body MQTTSubscriptionUpdateRequest) (*MQTTSubscription, error) {
	if err := validateMQTTSubscriptionID(id); err != nil {
		return nil, err
	}
	var out struct {
		Subscription MQTTSubscription `json:"subscription"`
	}
	_, err := c.mqttJSON(ctx, http.MethodPut, mqttSubscriptionPath(id), body, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	return &out.Subscription, nil
}

// DeleteMQTTSubscription calls DELETE /api/v1/mqtt/subscriptions/:id.
func (c *Client) DeleteMQTTSubscription(ctx context.Context, id string) error {
	if err := validateMQTTSubscriptionID(id); err != nil {
		return err
	}
	_, err := c.mqttJSON(ctx, http.MethodDelete, mqttSubscriptionPath(id), nil, 64<<10, nil)
	return err
}

// MQTTSubscriptionAction calls one lifecycle endpoint for a subscription.
func (c *Client) MQTTSubscriptionAction(ctx context.Context, id, action string) (string, error) {
	if err := validateMQTTSubscriptionID(id); err != nil {
		return "", err
	}
	switch action {
	case "start", "stop", "pause", "restart":
	default:
		return "", fmt.Errorf("unsupported MQTT subscription action %q", action)
	}
	var out struct {
		Message string `json:"message"`
	}
	_, err := c.mqttJSON(ctx, http.MethodPost, mqttSubscriptionPath(id)+"/"+action, nil, 64<<10, &out)
	if err != nil {
		return "", err
	}
	return out.Message, nil
}

// MQTTSubscriptionStats calls GET /api/v1/mqtt/subscriptions/:id/stats.
func (c *Client) MQTTSubscriptionStats(ctx context.Context, id string) (*MQTTSubscriptionStats, error) {
	if err := validateMQTTSubscriptionID(id); err != nil {
		return nil, err
	}
	var out struct {
		Stats MQTTSubscriptionStats `json:"stats"`
	}
	_, err := c.mqttJSON(ctx, http.MethodGet, mqttSubscriptionPath(id)+"/stats", nil, 1<<20, &out)
	if err != nil {
		return nil, err
	}
	return &out.Stats, nil
}

func mqttSubscriptionPath(id string) string {
	return mqttBase + "/subscriptions/" + url.PathEscape(id)
}

func validateMQTTSubscriptionID(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("subscription id is required")
	}
	return nil
}
