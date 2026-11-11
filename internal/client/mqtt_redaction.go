package client

import (
	"net/url"
	"regexp"
	"strings"
)

// Tolerate malformed URL escapes: those are precisely what a validation error
// may echo back. Never depend on url.Parse succeeding to hide credentials.
var mqttUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/"<>]*@`)
var mqttQuerySecret = regexp.MustCompile(`(?i)([?&;](?:token|access_token|refresh_token|password|passwd|pwd|secret|client_secret|api_key|apikey|key|sig|signature|x-amz-signature|x-amz-credential|x-amz-security-token)=)[^&;\s"<>]*`)

type mqttSafeError struct {
	cause   error
	message string
}

func (e *mqttSafeError) Error() string { return e.message }
func (e *mqttSafeError) Unwrap() error { return e.cause }

// ScrubMQTTBroker removes userinfo and sensitive query parameter values.
func ScrubMQTTBroker(broker string) string {
	if _, err := url.Parse(broker); err != nil && strings.Contains(broker, "@") {
		return "<redacted>"
	}
	return scrubMQTTPatterns(broker)
}

var mqttQueryPair = regexp.MustCompile(`[?&;][^=&;\s"<>]+=[^&;\s"<>]*`)

func scrubMQTTPatterns(broker string) string {
	broker = mqttQueryPair.ReplaceAllStringFunc(broker, func(pair string) string {
		i := strings.IndexByte(pair, '=')
		if i+1 == len(pair) {
			return pair
		}
		key, err := url.QueryUnescape(pair[1:i])
		if err == nil && mqttQuerySecret.MatchString("?"+key+"=value") {
			return pair[:i+1] + "<redacted>"
		}
		return pair
	})
	broker = mqttUserinfo.ReplaceAllString(broker, "$1")
	return broker
}
func MQTTBrokerHasCredentials(broker string) bool {
	if mqttUserinfo.MatchString(broker) || mqttQuerySecret.MatchString(broker) {
		return true
	}
	u, err := url.Parse(broker)
	if err != nil {
		return strings.Contains(broker, "@") || strings.Contains(broker, "?")
	}
	if u.User != nil {
		return true
	}
	for key := range u.Query() {
		if mqttQuerySecret.MatchString("?" + key + "=value") {
			return true
		}
	}
	return false
}
func mqttRequestSecrets(req any) []string {
	var broker, password string
	switch r := req.(type) {
	case MQTTSubscriptionCreateRequest:
		broker, password = r.Broker, r.Password
	case MQTTSubscriptionUpdateRequest:
		if r.Broker != nil {
			broker = *r.Broker
		}
		if r.Password != nil {
			password = *r.Password
		}
	}
	secrets := []string{password}
	if broker != "" && MQTTBrokerHasCredentials(broker) {
		secrets = append(secrets, broker)
	}
	return secrets
}
func scrubMQTTText(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "<redacted>")
		}
	}
	return scrubMQTTPatterns(text)
}
func scrubMQTTValue(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		return ScrubMQTTBroker(x)
	case []any:
		for i := range x {
			x[i] = scrubMQTTValue(x[i], secrets)
		}
	case map[string]any:
		for k, value := range x {
			if text, ok := value.(string); ok && (k == "error" || k == "error_message") {
				x[k] = scrubMQTTText(text, secrets)
			} else {
				x[k] = scrubMQTTValue(value, secrets)
			}
		}
	}
	return v
}
