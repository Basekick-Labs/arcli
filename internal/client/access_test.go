package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every body Arc's database listing routes answer a refusal with. All the
// 403s share a status code and differ only in message, so the message is
// the whole discriminator — and they do not mean the same thing: "no
// grant for that database" wants a different database, "no read
// permission" wants a different token, and "permission data unavailable"
// is a server fault that says nothing about either.
const (
	// The read middleware's RBAC denial, which is what these routes
	// return in practice. extractDatabase supplies the name; it is empty
	// for the list-everything route, which sends no database.
	mwScopedBody    = `{"success":false,"error":"no permission for read on database 'metrics'"}`
	mwScopedAllBody = `{"success":false,"error":"no permission for read on database ''"}`
	// The handler's own gate, reachable when it checks a different
	// database/measurement pair than the middleware did.
	handlerScopedBody    = `{"error":"access denied: no read permission for database 'metrics'"}`
	handlerScopedAllBody = `{"error":"access denied: no read permission for database '*'"}`
	// Coarse refusals: the token has no read permission at all.
	coarseForbiddenBody = `{"success":false,"error":"token does not have 'read' permission"}`
	ossForbiddenBody    = `{"success":false,"error":"Permission denied: read required"}`
	// Arc could not load the token's grants and refused rather than guess.
	unavailableBody = `{"success":false,"error":"permission data unavailable"}`

	noTokenBody  = `{"success":false,"error":"Authentication required"}`
	badTokenBody = `{"success":false,"error":"Invalid or expired token"}`
)

// statusServer answers every request with one status and body.
func statusServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// tokenlessClient is freshClient without a bearer token, for the 401
// case where the connection itself has no credential.
func tokenlessClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func mustAccessDenied(t *testing.T, err error) *AccessDeniedError {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var ae *AccessDeniedError
	if !errors.As(err, &ae) {
		t.Fatalf("error is %T (%v); expected *AccessDeniedError", err, err)
	}
	return ae
}

// A scoped token asking for every database gets 403, and that is the
// expected answer rather than a failure to retry: Arc will not return a
// filtered list. The message has to say so and name the way forward.
func TestListDatabases_ScopedForbidden(t *testing.T) {
	srv := statusServer(t, http.StatusForbidden, mwScopedAllBody)
	c := freshClient(t, srv, "")

	_, err := c.ListDatabases(context.Background())
	ae := mustAccessDenied(t, err)

	if ae.Status != http.StatusForbidden {
		t.Errorf("Status = %d; want 403", ae.Status)
	}
	if !ae.Scoped {
		t.Error("Scoped = false; the RBAC denial body must classify as scoped")
	}
	if ae.Database != listAllDatabases {
		t.Errorf("Database = %q; want %q for the list-everything route", ae.Database, listAllDatabases)
	}
	if !Scoped(err) {
		t.Error("Scoped(err) = false; callers must be able to recognise this state")
	}
	msg := err.Error()
	for _, want := range []string{"scoped to specific databases", "arcli db show"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

// A token with no read permission at all is a different problem, and
// telling its holder to "name a database" would send them down the
// wrong path. It must say the token lacks read permission.
func TestListDatabases_CoarseForbidden(t *testing.T) {
	srv := statusServer(t, http.StatusForbidden, coarseForbiddenBody)
	c := freshClient(t, srv, "")

	_, err := c.ListDatabases(context.Background())
	ae := mustAccessDenied(t, err)

	if ae.Scoped {
		t.Error("Scoped = true; the coarse permission denial is not a scoping state")
	}
	if Scoped(err) {
		t.Error("Scoped(err) = true for a coarse denial")
	}
	msg := err.Error()
	if !strings.Contains(msg, "read permission") {
		t.Errorf("message %q does not name the missing permission", msg)
	}
	if strings.Contains(msg, "scoped to specific databases") {
		t.Errorf("message %q misreports a missing permission as a scoping state", msg)
	}
}

// 401 on a connection that never had a token should say that, not quote
// a bare status code — the fix is to configure one.
func TestListDatabases_UnauthorizedWithoutToken(t *testing.T) {
	srv := statusServer(t, http.StatusUnauthorized, noTokenBody)
	c := tokenlessClient(t, srv)

	_, err := c.ListDatabases(context.Background())
	ae := mustAccessDenied(t, err)

	if ae.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d; want 401", ae.Status)
	}
	if ae.HasToken {
		t.Error("HasToken = true on a connection with no token")
	}
	if msg := err.Error(); !strings.Contains(msg, "this connection has none") {
		t.Errorf("message %q does not say the connection has no token", msg)
	}
}

// 401 with a token means the token is bad, which is a re-authentication
// problem and not a permissions one.
func TestListDatabases_UnauthorizedWithToken(t *testing.T) {
	srv := statusServer(t, http.StatusUnauthorized, badTokenBody)
	c := freshClient(t, srv, "")

	_, err := c.ListDatabases(context.Background())
	ae := mustAccessDenied(t, err)

	if !ae.HasToken {
		t.Error("HasToken = false; the connection does carry a token")
	}
	if ae.Scoped {
		t.Error("Scoped = true for a 401")
	}
	if msg := err.Error(); !strings.Contains(msg, "rejected this connection's token") {
		t.Errorf("message %q does not point at re-authentication", msg)
	}
}

// The per-database routes carry the database they were refused for, so
// the message can name it.
func TestGetDatabase_ScopedForbidden(t *testing.T) {
	srv := statusServer(t, http.StatusForbidden, mwScopedBody)
	c := freshClient(t, srv, "")

	_, err := c.GetDatabase(context.Background(), "metrics")
	ae := mustAccessDenied(t, err)

	if !ae.Scoped || ae.Database != "metrics" {
		t.Errorf("got Scoped=%v Database=%q; want true/\"metrics\"", ae.Scoped, ae.Database)
	}
	if msg := err.Error(); !strings.Contains(msg, `"metrics"`) {
		t.Errorf("message %q does not name the database", msg)
	}
}

func TestListMeasurements_ScopedForbidden(t *testing.T) {
	srv := statusServer(t, http.StatusForbidden, mwScopedBody)
	c := freshClient(t, srv, "")

	_, err := c.ListMeasurements(context.Background(), "metrics")
	ae := mustAccessDenied(t, err)

	if !ae.Scoped || ae.Database != "metrics" {
		t.Errorf("got Scoped=%v Database=%q; want true/\"metrics\"", ae.Scoped, ae.Database)
	}
}

// "No grant for this database" and "no such database" are different
// answers; 404 must stay an *HTTPError so the command layer keeps
// telling them apart.
func TestListingRoutes_NotFoundIsNotAccessDenied(t *testing.T) {
	srv := statusServer(t, http.StatusNotFound, `{"error":"Database 'ghost' not found"}`)
	c := freshClient(t, srv, "")

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"get", func() error { _, err := c.GetDatabase(context.Background(), "ghost"); return err }},
		{"measurements", func() error { _, err := c.ListMeasurements(context.Background(), "ghost"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			var ae *AccessDeniedError
			if errors.As(err, &ae) {
				t.Fatalf("404 classified as an access denial: %v", err)
			}
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != http.StatusNotFound {
				t.Fatalf("error is %T (%v); expected *HTTPError with Status 404", err, err)
			}
			if !strings.Contains(err.Error(), "not found") {
				t.Errorf("message %q does not say the database is missing", err.Error())
			}
		})
	}
}

// Everything that is not a 401 or 403 keeps its existing shape, so this
// change cannot swallow a server fault.
func TestListingRoutes_ServerErrorUnchanged(t *testing.T) {
	srv := statusServer(t, http.StatusInternalServerError, `{"error":"Failed to list databases: boom"}`)
	c := freshClient(t, srv, "")

	_, err := c.ListDatabases(context.Background())
	var ae *AccessDeniedError
	if errors.As(err, &ae) {
		t.Fatalf("500 classified as an access denial: %v", err)
	}
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != http.StatusInternalServerError {
		t.Fatalf("error is %T (%v); expected *HTTPError with Status 500", err, err)
	}
}

// An unrecognised 403 body must not be reported as a scoping state —
// telling an operator to name a database they already named is worse
// than naming the permission.
func TestListingRoutes_UnknownForbiddenBodyIsNotScoped(t *testing.T) {
	srv := statusServer(t, http.StatusForbidden, `{"error":"Delete operations are disabled."}`)
	c := freshClient(t, srv, "")

	_, err := c.GetDatabase(context.Background(), "metrics")
	ae := mustAccessDenied(t, err)
	if ae.Scoped {
		t.Errorf("Scoped = true for an unrecognised 403 body (%q)", ae.Message)
	}
}

// Arc answers these routes with 403 for three different reasons and
// distinguishes them only by message. Every shape the read middleware
// and the handler gate can produce has to land in the right bucket: a
// scoped token told to fix its permissions, or a permissionless token
// told to name a database, both send the operator the wrong way.
func TestClassifyAccessError_EveryForbiddenBody(t *testing.T) {
	// perDatabase picks the route the row exercises: the advice depends on
	// which route was called (name a database, or you have no grant for
	// the one you named), not on which database Arc happened to echo.
	tests := []struct {
		name            string
		body            string
		perDatabase     bool
		wantScoped      bool
		wantUnavailable bool
		wantInMessage   string
	}{
		{
			name: "middleware RBAC denial, named database", body: mwScopedBody,
			perDatabase: true, wantScoped: true, wantInMessage: "no read grant for database",
		},
		{
			name: "middleware RBAC denial, list-everything route sends no database",
			body: mwScopedAllBody, wantScoped: true, wantInMessage: "scoped to specific databases",
		},
		{
			name: "handler gate, named database", body: handlerScopedBody,
			perDatabase: true, wantScoped: true, wantInMessage: "no read grant for database",
		},
		{
			name: "handler gate, list-everything route", body: handlerScopedAllBody,
			wantScoped: true, wantInMessage: "scoped to specific databases",
		},
		{
			name: "coarse: token has no read permission", body: coarseForbiddenBody,
			wantInMessage: "does not carry the read permission",
		},
		{
			name: "coarse: RBAC not wired on this server", body: ossForbiddenBody,
			wantInMessage: "does not carry the read permission",
		},
		{
			name: "server could not load the grants", body: unavailableBody,
			wantUnavailable: true, wantInMessage: "server-side fault",
		},
		{
			name: "unrecognised body falls back to the coarse message",
			body: `{"error":"something new"}`, wantInMessage: "does not carry the read permission",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := statusServer(t, http.StatusForbidden, tt.body)
			c := freshClient(t, srv, "")

			var err error
			if tt.perDatabase {
				_, err = c.GetDatabase(context.Background(), "metrics")
			} else {
				_, err = c.ListDatabases(context.Background())
			}
			ae := mustAccessDenied(t, err)

			if ae.Scoped != tt.wantScoped {
				t.Errorf("Scoped = %v; want %v", ae.Scoped, tt.wantScoped)
			}
			if ae.Unavailable != tt.wantUnavailable {
				t.Errorf("Unavailable = %v; want %v", ae.Unavailable, tt.wantUnavailable)
			}
			if Scoped(err) != tt.wantScoped {
				t.Errorf("Scoped(err) = %v; want %v", Scoped(err), tt.wantScoped)
			}
			if msg := err.Error(); !strings.Contains(msg, tt.wantInMessage) {
				t.Errorf("message %q does not contain %q", msg, tt.wantInMessage)
			}
		})
	}
}

// The list-everything route sends no database name, so Arc's message
// carries an empty one. The advice still has to be "name a database",
// not "you have no grant for \"\"".
func TestListDatabases_EmptyDatabaseInDenialStillAsksForOne(t *testing.T) {
	srv := statusServer(t, http.StatusForbidden, mwScopedAllBody)
	c := freshClient(t, srv, "")

	_, err := c.ListDatabases(context.Background())
	msg := err.Error()
	if !strings.Contains(msg, "scoped to specific databases") {
		t.Errorf("message %q does not explain the scoping", msg)
	}
	if strings.Contains(msg, `""`) {
		t.Errorf("message %q quotes an empty database name", msg)
	}
}
