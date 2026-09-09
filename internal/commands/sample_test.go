package commands

import "testing"

func TestQuoteSQLIdentAndShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"citibike_trips", "citibike_trips"},
		{"trips2", "trips2"},
		{"Trips", `"Trips"`},
		{"my trips", `"my trips"`},
		{"a-b", `"a-b"`},
		{`ev"il`, `"ev""il"`},
		{"", `""`},
	} {
		if got := quoteSQLIdent(tc.in); got != tc.want {
			t.Errorf("quoteSQLIdent(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"nyc", "nyc"},
		{"my db", "'my db'"},
		{`SELECT "x"`, `'SELECT "x"'`},
		{"it's", `'it'\''s'`},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
