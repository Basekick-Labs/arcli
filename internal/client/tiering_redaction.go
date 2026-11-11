package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// Patterns copied from Arc's internal/storage/credential_text.go (#1403);
// keep them identical. Parse strings before scrubbing so escaped JSON cannot
// hide a signature; the patterns also match JSON-escaped text ("\u0026sig=").
var (
	tieringCredentialURL       = regexp.MustCompile(`(https?://[^\s"'<>?\\]*)\?(?:[^\s"'<>\\]|\\u0026)*`)
	tieringCredentialSignature = regexp.MustCompile(`(?i)(^|[^a-z])(SharedAccessSignature)=[^;\s"'<>\\]+`)
	tieringCredentialKey       = regexp.MustCompile(`(?i)(^|[^a-z])(sig|AccountKey)=[^&;\s"'<>\\]+`)
)

func redactTieringText(s string) string {
	s = tieringCredentialURL.ReplaceAllString(s, "$1")
	s = tieringCredentialSignature.ReplaceAllString(s, "${1}${2}=REDACTED")
	return tieringCredentialKey.ReplaceAllString(s, "${1}${2}=REDACTED")
}

// Work on RawMessage values to preserve unknown fields, large integers and nulls.
// Return the original bytes if nothing changed.
func redactTieringJSON(raw []byte) []byte {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if safe := redactTieringText(s); safe != s {
			b, _ := json.Marshal(safe)
			return b
		}
		return raw
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil && fields != nil {
		changed := false
		for key, value := range fields {
			safe := redactTieringJSON(value)
			if string(safe) != string(value) {
				changed = true
				fields[key] = safe
			}
		}
		if changed {
			b, _ := json.Marshal(fields)
			return b
		}
		return raw
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) == nil && values != nil {
		changed := false
		for i, value := range values {
			safe := redactTieringJSON(value)
			if string(safe) != string(value) {
				changed = true
				values[i] = safe
			}
		}
		if changed {
			b, _ := json.Marshal(values)
			return b
		}
		return raw
	}
	return raw
}

type tieringSafeError struct {
	text string
	err  error
}

func (e *tieringSafeError) Error() string { return e.text }
func (e *tieringSafeError) Unwrap() error { return e.err }

func (c *Client) tieringJSON(ctx context.Context, method, path string, req any, limit int64, out any) ([]byte, error) {
	raw, err := c.featureJSON(ctx, tieringFeature, tieringConfigKey, method, path, req, limit, nil)
	if err != nil {
		// Preserve status inspection and the shared route hint, without leaving
		// credentials accessible through HTTPError.Body or its error text.
		text := redactTieringText(err.Error())
		var he *HTTPError
		if errors.As(err, &he) {
			he.Body = redactTieringJSON(he.Body)
			if !json.Valid(he.Body) {
				he.Body = []byte(redactTieringText(string(he.Body)))
			}
			he.Message = redactTieringText(he.Message)
			he.Raw = redactTieringText(he.Raw)
			var detail struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(he.Body, &detail) == nil && detail.Message != "" && detail.Message != he.Message {
				text += "; " + scrubControls(detail.Message)
			}
		}
		return nil, &tieringSafeError{text: text, err: err}
	}
	raw = redactTieringJSON(raw)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}
	return raw, nil
}
