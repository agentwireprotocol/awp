// Package daemon runs an awp node as a long-lived local service and
// serves the control API that the CLI, the MCP server and harness hooks use.
//
// One daemon per awp home (~/.awp by default) owns the identity key,
// the tailcat listener and every connection, so switching harness mid-task
// keeps the identity and the conversations (spec section 15, question 1).
package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/agentwireprotocol/awp/internal/harness"
	"github.com/agentwireprotocol/awp/internal/node"
)

// Config is the daemon configuration: home/config.json, overridden by
// AWP_* environment variables, overridden by command-line flags.
type Config struct {
	Home    string `json:"-"`
	Name    string `json:"name,omitempty"`
	About   string `json:"about,omitempty"`
	Harness string `json:"harness,omitempty"` // claude, codex, ...: see internal/harness
	// DetectedHarness is the harness this daemon's environment points to.
	DetectedHarness string      `json:"-"`
	Model           string      `json:"model,omitempty"`      // the model the agent runs on
	ShareWith       []string    `json:"share_with,omitempty"` // keys of hosts to mirror conversations to (wire/mirror.go)
	Listen          []string    `json:"listen,omitempty"`
	Advertise       string      `json:"advertise,omitempty"`
	BlobLimit       int64       `json:"blob_limit,omitempty"`
	PingInterval    string      `json:"ping_interval,omitempty"`
	OutboxTTL       string      `json:"outbox_ttl,omitempty"`
	Policy          node.Policy `json:"policy,omitzero"`
	Trace           bool        `json:"trace,omitempty"`
	Plaintext       bool        `json:"allow_plaintext,omitempty"` // plain TCP to public addresses
	Verbose         bool        `json:"verbose,omitempty"`         // include tailcat's own logs
	Presence        bool        `json:"presence,omitempty"`        // publish signed presence (NOTES.md)
}

// DefaultHome is $AWP_HOME or ~/.awp.
func DefaultHome() string {
	if h := os.Getenv("AWP_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".awp"
	}
	return filepath.Join(home, ".awp")
}

// LoadConfig reads home/config.json (if present) and applies environment
// overrides.
func LoadConfig(home string) (Config, error) {
	cfg := Config{Home: home}
	b, err := os.ReadFile(filepath.Join(home, "config.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, errors.New("config.json: " + err.Error())
		}
		cfg.Home = home
	case !errors.Is(err, os.ErrNotExist):
		return cfg, err
	}
	env := func(k string) (string, bool) {
		v, ok := os.LookupEnv(k)
		return strings.TrimSpace(v), ok && strings.TrimSpace(v) != ""
	}
	list := func(v string) []string {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if v, ok := env("AWP_NAME"); ok {
		cfg.Name = v
	}
	if v, ok := env("AWP_ABOUT"); ok {
		cfg.About = v
	}
	if v, ok := env("AWP_SHARE_WITH"); ok {
		cfg.ShareWith = list(v)
	}
	if v, ok := env("AWP_MODEL"); ok {
		cfg.Model = v
	}
	if v, ok := env("AWP_HARNESS"); ok {
		cfg.Harness = harness.Normalize(v)
	}
	// Started by an agent: its harness marks the environment. This is only
	// a fallback; the node prefers a harness set explicitly, now or before.
	cfg.DetectedHarness = harness.Detect(os.Getenv)
	if v, ok := env("AWP_LISTEN"); ok {
		cfg.Listen = list(v)
	}
	if v, ok := env("AWP_ADVERTISE"); ok {
		cfg.Advertise = v
	}
	if v, ok := env("AWP_SERVE"); ok {
		cfg.Policy.Serve = list(v)
	}
	if v, ok := env("AWP_ROOT"); ok {
		cfg.Policy.Root = v
	}
	if v, ok := env("AWP_ACCEPT"); ok {
		cfg.Policy.Accept = v
	}
	if v, ok := env("AWP_ALLOW"); ok {
		cfg.Policy.Allow = append(cfg.Policy.Allow, list(v)...)
	}
	if v, ok := env("AWP_TRUST"); ok {
		cfg.Policy.Trust = append(cfg.Policy.Trust, list(v)...)
	}
	if v, ok := env("AWP_PING_INTERVAL"); ok {
		cfg.PingInterval = v
	}
	if v, ok := env("AWP_BLOB_LIMIT"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.BlobLimit = n
		}
	}
	if v, ok := env("AWP_TRACE"); ok && v != "0" {
		cfg.Trace = true
	}
	if v, ok := env("AWP_PRESENCE"); ok {
		cfg.Presence = v != "0" && v != "false"
	}
	if v, ok := env("AWP_ALLOW_PLAINTEXT"); ok && v != "0" {
		cfg.Plaintext = true
	}
	if len(cfg.Listen) == 0 {
		cfg.Listen = []string{"tailcat"}
	}
	return cfg, nil
}

// nodeConfig converts to the engine's configuration.
func (c Config) nodeConfig() (node.Config, error) {
	nc := node.Config{
		Home:            c.Home,
		Name:            c.Name,
		About:           c.About,
		Harness:         c.Harness,
		DetectedHarness: c.DetectedHarness,
		Model:           c.Model,
		ShareWith:       c.ShareWith,
		Listen:          c.Listen,
		Advertise:       c.Advertise,
		BlobLimit:       c.BlobLimit,
		Policy:          c.Policy,
		Trace:           c.Trace,

		AllowPlaintext: c.Plaintext,
		Presence:       c.Presence,
	}
	if c.PingInterval != "" {
		d, err := time.ParseDuration(c.PingInterval)
		if err != nil {
			return nc, errors.New("ping_interval: " + err.Error())
		}
		nc.PingInterval = d
	}
	if c.OutboxTTL != "" {
		d, err := time.ParseDuration(c.OutboxTTL)
		if err != nil {
			return nc, errors.New("outbox_ttl: " + err.Error())
		}
		nc.OutboxTTL = d
	}
	if nc.Policy.Root != "" {
		abs, err := filepath.Abs(nc.Policy.Root)
		if err != nil {
			return nc, err
		}
		nc.Policy.Root = abs
	}
	return nc, nil
}
