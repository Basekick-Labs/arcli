// Package config manages arcli's persistent client configuration.
//
// arcli stores named connections (endpoint + token + default database)
// in a TOML file at ~/.arcli/config.toml, with one connection marked
// "active". This mirrors the InfluxDB v2 CLI's `influx config` model so
// operators coming from InfluxDB get the UX without thinking.
//
// Precedence for which connection a command uses (highest first):
//  1. --connection / -c flag
//  2. --endpoint [+ --token] flags (ad-hoc override)
//  3. ARC_CONNECTION env var
//  4. ARC_ENDPOINT [+ ARC_TOKEN] env vars (ad-hoc override)
//  5. active connection in ~/.arcli/config.toml
//
// A token is optional everywhere: an Arc running with auth.enabled =
// false accepts requests without one, so a profile or an ad-hoc
// connection may carry an empty token and arcli then sends no
// Authorization header. A token without an endpoint is still an error.
// If nothing is set the resolver returns an error rather than guessing.
package config

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Connection is one named Arc endpoint + its credentials.
type Connection struct {
	Endpoint        string `mapstructure:"endpoint" toml:"endpoint"`
	Token           string `mapstructure:"token" toml:"token"`
	DefaultDatabase string `mapstructure:"default_database,omitempty" toml:"default_database,omitempty"`
	InsecureTLS     bool   `mapstructure:"insecure_tls,omitempty" toml:"insecure_tls,omitempty"`
}

// Config is the whole arcli config file's contents.
type Config struct {
	Active      string                `mapstructure:"active" toml:"active"`
	Connections map[string]Connection `mapstructure:"connections" toml:"connections"`

	// InstallationID is a random UUID minted by the first Save() and
	// kept for the life of the config file. arcli presents it to the
	// Arc servers it talks to (header Arcli-Installation-Id) so Arc's
	// own telemetry can count CLI installations; arcli itself never
	// sends it anywhere else. Delete the key to mint a new one.
	InstallationID string `mapstructure:"installation_id" toml:"installation_id"`

	// SendInstallationID gates the header. nil (key absent) means true,
	// so a Config built as a struct literal is never a silent opt-out;
	// `send_installation_id = false` in the file (or the DO_NOT_TRACK=1
	// env var) stops identifying this installation to servers.
	SendInstallationID *bool `mapstructure:"send_installation_id" toml:"send_installation_id,omitempty"`
}

// SendsInstallationID reports the config-file setting (absent = true).
func (c *Config) SendsInstallationID() bool {
	return c.SendInstallationID == nil || *c.SendInstallationID
}

// OutboundInstallationID is the id to put on requests, or "" when the
// user opted out (config key or DO_NOT_TRACK) or no id exists yet (no
// config file has ever been written, e.g. env-only use in a container).
func (c *Config) OutboundInstallationID() string {
	if !c.SendsInstallationID() || DoNotTrack() {
		return ""
	}
	return c.InstallationID
}

// DoNotTrack reports whether the DO_NOT_TRACK environment variable is
// set to a truthy value (https://consoledonottrack.com).
func DoNotTrack() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DO_NOT_TRACK"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// newInstallationID returns a random version-4 UUID.
func newInstallationID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate installation id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ConfigPath returns the path arcli reads/writes its config from.
// Honors ARCLI_CONFIG env var (for tests + CI); otherwise
// ~/.arcli/config.toml. Does not create anything; Save() makes the directory.
func ConfigPath() (string, error) {
	if p := os.Getenv("ARCLI_CONFIG"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".arcli", "config.toml"), nil
}

// Load reads the config file. Returns an empty (no-connections) Config
// if the file does not exist — this is the expected first-run state, not
// an error. Returns a real error only for malformed files or I/O
// failures.
func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return &Config{Connections: map[string]Connection{}}, nil
	} else if err != nil {
		return nil, fmt.Errorf("stat config: %w", err)
	}

	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("toml")
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := &Config{Connections: map[string]Connection{}}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if cfg.Connections == nil {
		cfg.Connections = map[string]Connection{}
	}
	return cfg, nil
}

// Save writes the config back to disk atomically (write to .tmp + rename).
// Creates parent dirs (mode 0700) and writes the file mode 0600 because
// tokens are plaintext — same posture as ~/.aws/credentials and the
// existing `arc.toml` server config.
func (c *Config) Save() error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	if c.InstallationID == "" {
		id, err := newInstallationID()
		if err != nil {
			return err
		}
		c.InstallationID = id
	}
	v := viper.New()
	v.Set("active", c.Active)
	v.Set("connections", c.Connections)
	v.Set("installation_id", c.InstallationID)
	if c.SendInstallationID != nil && !*c.SendInstallationID {
		// Only the opt-out is written; the default stays implicit.
		v.Set("send_installation_id", false)
	}

	// Write to a temp file in the same directory then rename atomically,
	// so we never leave a half-written config behind on a crash. Use a
	// .toml extension on the temp path because viper.WriteConfigAs infers
	// format from the extension and rejects unknown suffixes like .tmp.
	tmp, err := os.CreateTemp(dir, "config.*.toml")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := tmp.Name()
	// Close immediately — viper.WriteConfigAs reopens with its own
	// codec rather than appending to our handle.
	_ = tmp.Close()
	if err := v.WriteConfigAs(tmpPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename config into place: %w", err)
	}
	return nil
}

// ResolveOptions are the per-command overrides Resolve consults before
// falling back to env vars and the active connection in the file.
type ResolveOptions struct {
	// ConnectionName overrides which named connection to use. Empty
	// means "use precedence below".
	ConnectionName string
	// Endpoint + Token are full ad-hoc overrides. If both are set,
	// they produce an unnamed Connection without touching the file.
	Endpoint string
	Token    string
}

// Resolve returns the Connection a command should use. See the package
// docstring for full precedence rules. The returned Connection is never
// persisted by this call — Save() is invoked explicitly by the `config`
// subcommands and by `auth token rotate --save`.
func (c *Config) Resolve(opts ResolveOptions) (Connection, string, error) {
	// 1. --connection flag wins outright.
	if opts.ConnectionName != "" {
		conn, ok := c.Connections[opts.ConnectionName]
		if !ok {
			return Connection{}, "", fmt.Errorf("connection %q not found in config (use `arcli config list`)", opts.ConnectionName)
		}
		return conn, opts.ConnectionName, nil
	}

	// 2. Ad-hoc flag override: --endpoint, with --token when the server
	// requires one. A token alone names no server and is an error.
	if opts.Endpoint != "" {
		return Connection{Endpoint: opts.Endpoint, Token: opts.Token}, "(flags)", nil
	}
	if opts.Token != "" {
		return Connection{}, "", errors.New("--token needs --endpoint (or use a named connection)")
	}

	// 3. ARC_CONNECTION env var.
	if name := os.Getenv("ARC_CONNECTION"); name != "" {
		conn, ok := c.Connections[name]
		if !ok {
			return Connection{}, "", fmt.Errorf("ARC_CONNECTION=%q not found in config", name)
		}
		return conn, name, nil
	}

	// 4. Env override: ARC_ENDPOINT, with ARC_TOKEN when the server
	// requires one.
	ep := os.Getenv("ARC_ENDPOINT")
	tok := os.Getenv("ARC_TOKEN")
	if ep != "" {
		return Connection{Endpoint: ep, Token: tok}, "(env)", nil
	}
	if tok != "" {
		return Connection{}, "", errors.New("ARC_TOKEN needs ARC_ENDPOINT (or use ARC_CONNECTION)")
	}

	// 5. Active connection in file.
	if c.Active == "" {
		return Connection{}, "", errors.New("no active connection configured (run `arcli config create --name NAME --endpoint URL [--token TOKEN] --activate`)")
	}
	conn, ok := c.Connections[c.Active]
	if !ok {
		return Connection{}, "", fmt.Errorf("active connection %q referenced in config but not defined", c.Active)
	}
	return conn, c.Active, nil
}

// RedactToken returns the token with all but the first 4 and last 4
// characters replaced by * for display in `config list` / `config
// current` output. A token shorter than 12 chars is fully redacted; an
// empty token (a profile for a server without authentication) reads
// "(none)".
func RedactToken(token string) string {
	if token == "" {
		return "(none)"
	}
	if len(token) < 12 {
		return "************"
	}
	return token[:4] + "..." + token[len(token)-4:]
}
