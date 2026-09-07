package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newAuthTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	cli, err := New(Config{Endpoint: srv.URL, Token: "test-token", Database: "ignored_default"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return cli, srv
}

// Every auth route must send Bearer auth and must NOT send
// x-arc-database even though the client has a default database.
func assertAuthHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get(HeaderDatabase); got != "" {
		t.Errorf("x-arc-database leaked onto auth route: %q", got)
	}
}

func TestListTokens_NullBecomesEmptySlice(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.URL.Path != "/api/v1/auth/tokens" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"success":true,"tokens":null,"count":0}`))
	})
	tokens, err := cli.ListTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tokens == nil || len(tokens) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", tokens)
	}
	b, _ := json.Marshal(tokens)
	if string(b) != "[]" {
		t.Errorf("JSON = %s, want []", b)
	}
}

func TestListTokens_DecodesTokenInfo(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"tokens":[
		  {"id":1,"name":"admin","permissions":["read","write","delete","admin"],"created_at":"2026-09-07T10:00:00Z","enabled":true,"expires_at":null},
		  {"id":2,"name":"ci","description":"d","permissions":null,"created_at":"2026-09-07T11:00:00Z","last_used_at":"2026-09-07T12:00:00Z","enabled":false,"expires_at":"2026-10-07T11:00:00Z"}
		],"count":2}`))
	})
	tokens, err := cli.ListTokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 {
		t.Fatalf("len = %d", len(tokens))
	}
	if tokens[0].ExpiresAt != nil || tokens[0].LastUsedAt != nil || !tokens[0].Enabled {
		t.Errorf("token 0 = %+v", tokens[0])
	}
	if tokens[1].Permissions == nil || len(tokens[1].Permissions) != 0 {
		t.Errorf("null permissions must normalise to empty slice, got %#v", tokens[1].Permissions)
	}
	if tokens[1].ExpiresAt == nil || tokens[1].ExpiresAt.Year() != 2026 || tokens[1].Enabled {
		t.Errorf("token 1 = %+v", tokens[1])
	}
	if !tokens[1].IsExpired(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) || tokens[1].IsExpired(time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)) {
		t.Error("IsExpired wrong")
	}
}

func TestGetToken_404IsHTTPError(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/tokens/42" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":"Token not found"}`))
	})
	_, err := cli.GetToken(context.Background(), 42)
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 404 || he.Message != "Token not found" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "Token not found (HTTP 404)") {
		t.Errorf("Error() = %q", err)
	}
}

func TestCreateToken_ReturnsSecretAndSendsBody(t *testing.T) {
	var got CreateTokenOptions
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/tokens" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"token":"s3cret==","message":"store it"}`))
	})
	perms := []string{"read"}
	secret, err := cli.CreateToken(context.Background(), CreateTokenOptions{Name: "ci", Permissions: &perms, ExpiresIn: "7d"})
	if err != nil {
		t.Fatal(err)
	}
	if secret != "s3cret==" {
		t.Errorf("secret = %q", secret)
	}
	if got.Name != "ci" || got.Permissions == nil || (*got.Permissions)[0] != "read" || got.ExpiresIn != "7d" {
		t.Errorf("body = %+v", got)
	}
}

// nil permissions must be OMITTED (server default); empty slice must be
// sent as [] (RBAC-only). The distinction is the whole point of the
// pointer-to-slice type.
func TestCreateToken_PermissionsNilVsEmpty(t *testing.T) {
	var raw map[string]json.RawMessage
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw = nil
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"success":true,"token":"x"}`))
	})
	if _, err := cli.CreateToken(context.Background(), CreateTokenOptions{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["permissions"]; present {
		t.Errorf("nil permissions must be omitted, got %s", raw["permissions"])
	}
	empty := []string{}
	if _, err := cli.CreateToken(context.Background(), CreateTokenOptions{Name: "b", Permissions: &empty}); err != nil {
		t.Fatal(err)
	}
	if string(raw["permissions"]) != "[]" {
		t.Errorf("empty permissions must be sent as [], got %s", raw["permissions"])
	}
}

func TestCreateToken_ClientSideValidation(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent")
	})
	bad := []string{"root"}
	if _, err := cli.CreateToken(context.Background(), CreateTokenOptions{Name: "x", Permissions: &bad}); err == nil || !strings.Contains(err.Error(), `invalid permission "root"`) {
		t.Errorf("err = %v", err)
	}
	if _, err := cli.CreateToken(context.Background(), CreateTokenOptions{Name: "x", ExpiresIn: "1w"}); err == nil {
		t.Error("1w should be rejected")
	}
	if _, err := cli.CreateToken(context.Background(), CreateTokenOptions{Name: ""}); err == nil {
		t.Error("empty name should be rejected")
	}
}

func TestParseExpiresIn(t *testing.T) {
	ok := map[string]time.Duration{"24h": 24 * time.Hour, "90m": 90 * time.Minute, "7d": 7 * 24 * time.Hour, "1d": 24 * time.Hour}
	for in, want := range ok {
		got, err := ParseExpiresIn(in)
		if err != nil || got != want {
			t.Errorf("ParseExpiresIn(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "0d", "-1h", "1w", "7 days", "1d12h", "d", "abc"} {
		if _, err := ParseExpiresIn(in); err == nil {
			t.Errorf("ParseExpiresIn(%q) should fail", in)
		}
	}
}

func TestUpdateToken_EmptyRefusedClientSide(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be sent")
	})
	err := cli.UpdateToken(context.Background(), 1, UpdateTokenOptions{})
	if err == nil || !strings.Contains(err.Error(), "nothing to update") {
		t.Errorf("err = %v", err)
	}
}

func TestUpdateToken_SendsPatchWithOnlyChangedFields(t *testing.T) {
	var raw map[string]json.RawMessage
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/api/v1/auth/tokens/7" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte(`{"success":true,"message":"Token updated successfully"}`))
	})
	desc := ""
	if err := cli.UpdateToken(context.Background(), 7, UpdateTokenOptions{Description: &desc}); err != nil {
		t.Fatal(err)
	}
	if string(raw["description"]) != `""` {
		t.Errorf("empty description must be sent to clear it, got %s", raw["description"])
	}
	for _, absent := range []string{"name", "permissions", "expires_in"} {
		if _, ok := raw[absent]; ok {
			t.Errorf("%s must be omitted when unchanged", absent)
		}
	}
}

func TestRotateToken_ReadsNewTokenKey(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/tokens/3/rotate" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		// The server uses "new_token" here, NOT "token" as on create.
		_, _ = w.Write([]byte(`{"success":true,"new_token":"rotated==","message":"..."}`))
	})
	secret, err := cli.RotateToken(context.Background(), 3)
	if err != nil || secret != "rotated==" {
		t.Fatalf("secret=%q err=%v", secret, err)
	}
}

func TestRotateToken_MissingKeyIsError(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"token":"wrong-key"}`))
	})
	if _, err := cli.RotateToken(context.Background(), 3); err == nil {
		t.Fatal("a rotate response without new_token must be an error, not an empty secret")
	}
}

func TestRevokeAndDelete_Paths(t *testing.T) {
	var seen []string
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	if err := cli.RevokeToken(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	if err := cli.DeleteToken(context.Background(), 6); err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "POST /api/v1/auth/tokens/5/revoke,DELETE /api/v1/auth/tokens/6" {
		t.Errorf("seen = %v", seen)
	}
}

func TestVerify_OKAnd401(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuthHeaders(t, r)
		if r.URL.Path != "/api/v1/auth/verify" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"valid":true,"token_info":{"id":1,"name":"admin","permissions":["admin"],"created_at":"2026-09-07T10:00:00Z","enabled":true,"expires_at":null},"permissions":["admin"]}`))
	})
	me, err := cli.Verify(context.Background())
	if err != nil || me.ID != 1 || me.Name != "admin" {
		t.Fatalf("me=%+v err=%v", me, err)
	}

	cli2, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"valid":false,"error":"Invalid or expired token"}`))
	})
	_, err = cli2.Verify(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 401 || he.Message != "Invalid or expired token" {
		t.Fatalf("err = %v", err)
	}
}

func TestGetTokenPermissions_NullNormalised(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/tokens/9/permissions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"success":true,"permissions":null,"rbac_enabled":false}`))
	})
	p, err := cli.GetTokenPermissions(context.Background(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if p.Permissions == nil || len(p.Permissions) != 0 || p.RBACEnabled {
		t.Errorf("got %+v", p)
	}
}

func TestHealth_NoAuthHeaderAndPassThrough(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("/health must not receive the token")
		}
		_, _ = w.Write([]byte(`{"status":"ok","time":"t","uptime":"5s","uptime_sec":5,"storage":{"hot":{"backend":"local","state":"ok"}},"license":{"tier":"oss","status":"unlicensed"}}`))
	})
	h, err := cli.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.Uptime != "5s" || h.Latency <= 0 {
		t.Errorf("h = %+v", h)
	}
	if !strings.Contains(string(h.Storage), `"hot"`) || !strings.Contains(string(h.License), `"oss"`) {
		t.Errorf("raw pass-through lost: storage=%s license=%s", h.Storage, h.License)
	}
}

func TestHealth_NonJSONBodyIsClearError(t *testing.T) {
	cli, _ := newAuthTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>proxy says hi</html>`))
	})
	_, err := cli.Health(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not an Arc health response") {
		t.Errorf("err = %v", err)
	}
}
