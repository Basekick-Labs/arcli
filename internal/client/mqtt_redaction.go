package client

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Tolerate malformed URL escapes: those are precisely what a validation error
// may echo back. Never depend on url.Parse succeeding to hide credentials.
// In free text the userinfo match is bounded by whitespace and quotes; it may
// over-redact a token, never under-redact one that has a scheme.
var mqttTextUserinfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s"<>]*@`)

var mqttQueryPair = regexp.MustCompile(`[?&;][^=&;\s"<>]+=[^&;\s"<>]*`)

// mqttSensitiveKeyParts are matched as substrings of the decoded, lowercased
// query key, so auth_token, private_key, X-Amz-Signature and jwt are all
// covered. Over-redacting a harmless parameter is the safe failure.
var mqttSensitiveKeyParts = []string{"token", "pass", "pwd", "secret", "key", "sig", "auth", "cred", "jwt", "session"}

type mqttSafeError struct {
	cause   error
	message string
}

func (e *mqttSafeError) Error() string { return e.message }
func (e *mqttSafeError) Unwrap() error { return e.cause }

func mqttSensitiveKey(raw string) bool {
	key, err := url.QueryUnescape(raw)
	if err != nil {
		key = raw
	}
	key = strings.ToLower(key)
	for _, part := range mqttSensitiveKeyParts {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

// mqttUserinfoSpan returns the byte range [start, at) of a userinfo-like
// segment in a broker URL: everything after "://" up to the last '@' that is
// not inside a query value. It does not rely on url.Parse, because a password
// containing '/', '?', '#' or '"' makes url.Parse either fail or put part of
// the password in the host (tcp://u:1234/ss@host parses with host "u:1234").
// at is -1 when there is no such segment.
func mqttUserinfoSpan(broker string) (start, at int) {
	i := strings.Index(broker, "://")
	if i < 0 {
		return 0, -1
	}
	start = i + 3
	rest := broker[start:]
	for j := strings.LastIndexByte(rest, '@'); j >= 0; j = strings.LastIndexByte(rest[:j], '@') {
		if !mqttInQueryValue(rest, j) {
			return start, start + j
		}
	}
	return start, -1
}

// mqttInQueryValue reports whether rest[j] lies in the value of a key=value
// query pair, e.g. the '@' in ws://host/mqtt?user=a@b.
func mqttInQueryValue(rest string, j int) bool {
	q := strings.IndexByte(rest, '?')
	if q < 0 || q > j {
		return false
	}
	seg := strings.LastIndexAny(rest[:j], "?&;")
	return strings.Contains(rest[seg:j], "=")
}

// ScrubMQTTBroker removes userinfo and sensitive query parameter values.
func ScrubMQTTBroker(broker string) string {
	if start, at := mqttUserinfoSpan(broker); at >= 0 {
		broker = broker[:start] + broker[at+1:]
	}
	return scrubMQTTQuery(broker)
}

func scrubMQTTQuery(s string) string {
	return mqttQueryPair.ReplaceAllStringFunc(s, func(pair string) string {
		i := strings.IndexByte(pair, '=')
		if i+1 == len(pair) || !mqttSensitiveKey(pair[1:i]) {
			return pair
		}
		return pair[:i+1] + "<redacted>"
	})
}

func scrubMQTTPatterns(text string) string {
	text = scrubMQTTQuery(text)
	return mqttTextUserinfo.ReplaceAllString(text, "$1")
}

// MQTTBrokerHasCredentials reports whether a broker URL carries userinfo or a
// sensitive query parameter.
func MQTTBrokerHasCredentials(broker string) bool {
	if _, at := mqttUserinfoSpan(broker); at >= 0 {
		return true
	}
	for _, pair := range mqttQueryPair.FindAllString(broker, -1) {
		if mqttSensitiveKey(pair[1:strings.IndexByte(pair, '=')]) {
			return true
		}
	}
	if _, err := url.Parse(broker); err != nil {
		return strings.Contains(broker, "?")
	}
	return false
}

// mqttRequestSecrets returns the literal secrets a create/update request
// carries, and whether its broker URL carries credentials.
func mqttRequestSecrets(req any) ([]string, bool) {
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
	brokerCreds := broker != "" && MQTTBrokerHasCredentials(broker)
	if brokerCreds {
		// Go's url.Error quotes the URL with %q, so a broker containing '"'
		// or '\' is echoed escaped; replace both spellings.
		quoted := strconv.Quote(broker)
		secrets = append(secrets, broker, quoted[1:len(quoted)-1])
		if start, at := mqttUserinfoSpan(broker); at > start {
			secrets = append(secrets, broker[start:at])
		}
	}
	return secrets, brokerCreds
}

func scrubMQTTText(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "<redacted>")
		}
	}
	return scrubMQTTPatterns(text)
}

// withholdMQTTBrokerParseDetail drops the detail of Arc's
// `invalid broker URL: parse "...": <reason>` error. url.Parse reasons quote
// pieces of the URL (`invalid port ":pa" after host`, `invalid URL escape
// "%zz"`), and for a credential-bearing URL those pieces can be password
// fragments that no literal replacement can find. Arc's other broker URL
// errors (scheme, missing host) echo nothing and are kept.
func withholdMQTTBrokerParseDetail(msg string) string {
	const marker = "invalid broker URL"
	i := strings.Index(msg, marker)
	if i < 0 || !strings.Contains(msg[i:], "parse ") {
		return msg
	}
	end := len(msg)
	if j := strings.LastIndex(msg, " (HTTP "); j > i {
		end = j
	}
	return msg[:i] + marker + ": it cannot be parsed (detail withheld because the URL contains credentials; percent-encode reserved characters such as / ? # @ % in the username and password)" + msg[end:]
}

func scrubMQTTValue(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		// Only URL-shaped strings are broker candidates; names, topics and
		// mapping targets keep their exact value.
		if strings.Contains(x, "://") {
			return ScrubMQTTBroker(x)
		}
		return x
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
