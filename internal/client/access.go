package client

import (
	"errors"
	"fmt"
	"strings"
)

// AccessDeniedError is Arc's refusal of a read on one of the database
// listing routes — GET /api/v1/databases, /api/v1/databases/:name and
// /api/v1/databases/:name/measurements.
//
// Those three routes used to carry no authentication at all: any valid
// token, and on some deployments no token, could enumerate every
// database and measurement name on the server. They now require the
// read permission and, where the server restricts reads per database, a
// read grant covering the database being listed. So a 401 or 403 from
// them is new, and the 403s do not all mean the same thing:
//
//   - Arc refuses a token whose grants do not cover the database asked
//     for. That is a NORMAL state for a tenant-scoped token, not a
//     failure: the caller has to name a database it does hold a grant
//     for. Arc will not answer the list-everything route with a
//     filtered list — a scoped caller must name its database, the same
//     way `SHOW DATABASES` behaves — so a 403 there is the expected
//     answer and must not be retried.
//   - Arc refuses a token that does not carry the "read" permission at
//     all. That needs a different token; naming a database will not
//     help.
//   - Arc fails closed when it cannot read its own permission data.
//     That is a server-side fault, not a statement about the token.
//
// Those three are told apart by the message Arc sends, because the
// status code is 403 for all of them. See classifyAccessError for the
// shapes and for what an unrecognised one is reported as.
type AccessDeniedError struct {
	// Status is 401 or 403 as Arc returned it.
	Status int
	// Database is the database the caller asked about, or
	// listAllDatabases for the list-everything route.
	Database string
	// Scoped is true when Arc refused because the token's grants do not
	// cover the database asked for — "name a database you are granted".
	// Only meaningful for Status 403.
	Scoped bool
	// Unavailable is true when Arc could not read its own permission
	// data and failed closed. Nothing about the token or the database
	// caused it. Only meaningful for Status 403.
	Unavailable bool
	// HasToken records whether the connection sent a bearer token at
	// all, so a 401 on a token-less connection can say so instead of
	// quoting a bare status code.
	HasToken bool
	// Message is Arc's own message, already control-scrubbed by
	// decodeWriteError.
	Message string
}

// listAllDatabases is the pseudo-name the server uses for the
// list-everything route's permission check, and the value Database
// carries for it.
const listAllDatabases = "*"

func (e *AccessDeniedError) Error() string {
	switch {
	case e.Status == 401 && !e.HasToken:
		return "Arc requires a token to list databases but this connection has none " +
			"(add one with `arcli config update NAME --token ...`)"
	case e.Status == 401:
		return fmt.Sprintf("Arc rejected this connection's token (%s); "+
			"it may be invalid, expired or revoked — issue a new one and "+
			"`arcli config update NAME --token ...`", e.Message)
	case e.Unavailable:
		return fmt.Sprintf("arc could not read its own permission data and refused the "+
			"request rather than guessing (%s); this is a server-side fault, not a "+
			"problem with this token — check the server log", e.Message)
	case e.Scoped && e.Database == listAllDatabases:
		return "this token is scoped to specific databases, so Arc will not list them all; " +
			"name a database you are granted (`arcli db show <database>`, " +
			"`arcli measurement list --database <database>`)"
	case e.Scoped:
		return fmt.Sprintf("this token has no read grant for database %q; "+
			"name a database it is granted, or ask an Arc administrator to grant it",
			e.Database)
	default:
		return fmt.Sprintf("this token does not carry the read permission Arc requires "+
			"to list databases (%s); use a token with read permission", e.Message)
	}
}

// Scoped reports whether err is Arc's per-database RBAC denial — the
// "you are scoped, name your database" answer — rather than a missing
// read permission or a bad token. Exposed so a caller can treat that
// case as a normal, expected state.
func Scoped(err error) bool {
	var ae *AccessDeniedError
	return errors.As(err, &ae) && ae.Scoped
}

// classifyAccessError maps the 401/403 the database listing routes can
// answer with onto *AccessDeniedError and leaves every other error
// untouched. database is the name asked about, or listAllDatabases for
// the list-everything route.
//
// Arc sends 403 for three different things and distinguishes them only
// by message, so the message is what this reads. The shapes, all from
// the read middleware and the handler gate on these routes:
//
//	no permission for read on database 'x'                -> scoped
//	access denied: no read permission for database 'x'    -> scoped
//	permission data unavailable                           -> unavailable
//	token does not have 'read' permission                 -> coarse
//	Permission denied: read required                      -> coarse
//
// Matching is by stable fragment rather than whole string, so a change
// to the permission word or to surrounding punctuation does not
// silently reclassify one for another. An unrecognised 403 is reported
// as the coarse case on purpose: naming the token's permissions is a
// safe thing to say about any refusal, whereas telling an operator to
// pass a database they may already have passed is not.
func (c *Client) classifyAccessError(err error, database string) error {
	var he *HTTPError
	if !errors.As(err, &he) {
		return err
	}
	if he.Status != 401 && he.Status != 403 {
		return err
	}
	msg := he.Message
	if msg == "" {
		msg = he.Raw
	}
	return &AccessDeniedError{
		Status:      he.Status,
		Database:    database,
		Scoped:      he.Status == 403 && isScopedDenial(msg),
		Unavailable: he.Status == 403 && isPermissionDataUnavailable(msg),
		HasToken:    c.HasToken(),
		Message:     msg,
	}
}

// isScopedDenial recognises the two bodies that mean "your grants do not
// cover that database": the read middleware's
// "no permission for read on database 'x'" and the handler gate's
// "access denied: no read permission for database 'x'".
func isScopedDenial(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	switch {
	case strings.HasPrefix(m, "no permission for ") && strings.Contains(m, " on database "):
		return true
	case strings.HasPrefix(m, "access denied: no ") && strings.Contains(m, "permission for database"):
		return true
	default:
		return false
	}
}

// isPermissionDataUnavailable recognises Arc's fail-closed answer when
// it could not load the token's grants. It is a 403 like the others but
// says nothing about the token, so it must not be reported as one.
func isPermissionDataUnavailable(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "permission data unavailable")
}
