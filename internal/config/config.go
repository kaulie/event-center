// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Listen-address sources, in precedence order. EVENTD_HTTP_ADDR is this
// service's own knob (host:port); SERVICE_PORT is the deployment platform
// convention — it injects the contract's port into every start/stop/restart
// command, so honouring it is what makes a plain "deploy the merged commit"
// land on the right port without a service-local override.
const (
	EnvHTTPAddr    = "EVENTD_HTTP_ADDR"
	EnvServicePort = "SERVICE_PORT"

	// DefaultHTTPAddr is used when neither variable yields a usable value.
	DefaultHTTPAddr = ":8080"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	HTTPAddr string
	// HTTPAddrSource records where HTTPAddr came from (EnvHTTPAddr,
	// EnvServicePort or "default"), so the start-up log names the knob that is
	// actually in effect instead of leaving it to be guessed.
	HTTPAddrSource string
	// HTTPAddrWarning explains what was ignored when a listen-address variable
	// was set but unusable (empty when everything was fine). Load deliberately
	// does not log: it runs before main installs the JSON handler, so logging
	// here would emit an unstructured line into an otherwise JSON log stream.
	// The caller reports it once the real logger exists.
	HTTPAddrWarning string
	DBPath          string
	AdminToken      string
	GitHubSecret    string
	RetentionDays   int
	PullWaitMax     time.Duration
	PullDefaultSize int
	PullMaxSize     int

	PushEnabled        bool
	PushTimeout        time.Duration
	PushBatchSize      int
	PushMaxAttempts    int
	PushBaseBackoff    time.Duration
	PushMaxBackoff     time.Duration
	PushPollInterval   time.Duration
	DispatcherDisabled bool

	// Ingress audit trail. Rejected requests have no other copy anywhere, so
	// they are always logged in enough detail to trace why data did not arrive.
	IngressLogBody    bool   // log raw bodies of accepted requests too
	IngressLogBodyMax int    // truncate captured bodies to this many bytes
	IngressLogPath    string // append JSONL audit file ("" = journald only)

	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		DBPath:             env("EVENTD_DB_PATH", "./data/eventd.db"),
		AdminToken:         env("EVENTD_ADMIN_TOKEN", ""),
		GitHubSecret:       env("EVENTD_GITHUB_SECRET", ""),
		RetentionDays:      envInt("EVENTD_RETENTION_DAYS", 0),
		PullDefaultSize:    envInt("EVENTD_PULL_DEFAULT_LIMIT", 100),
		PullMaxSize:        envInt("EVENTD_PULL_MAX_LIMIT", 1000),
		PushEnabled:        envBool("EVENTD_PUSH_ENABLED", true),
		PushBatchSize:      envInt("EVENTD_PUSH_BATCH_SIZE", 50),
		PushMaxAttempts:    envInt("EVENTD_PUSH_MAX_ATTEMPTS", 6),
		DispatcherDisabled: envBool("EVENTD_DISPATCHER_DISABLED", false),
		IngressLogBody:     envBool("EVENTD_INGRESS_LOG_BODY", false),
		IngressLogBodyMax:  envInt("EVENTD_INGRESS_LOG_BODY_MAX", 8192),
		IngressLogPath:     env("EVENTD_INGRESS_LOG_PATH", ""),
	}

	c.HTTPAddr, c.HTTPAddrSource, c.HTTPAddrWarning = resolveHTTPAddr()

	var err error
	if c.PullWaitMax, err = envDuration("EVENTD_PULL_WAIT_MAX", 30*time.Second); err != nil {
		return nil, err
	}
	if c.PushTimeout, err = envDuration("EVENTD_PUSH_TIMEOUT", 10*time.Second); err != nil {
		return nil, err
	}
	if c.PushBaseBackoff, err = envDuration("EVENTD_PUSH_BASE_BACKOFF", 2*time.Second); err != nil {
		return nil, err
	}
	if c.PushMaxBackoff, err = envDuration("EVENTD_PUSH_MAX_BACKOFF", 10*time.Minute); err != nil {
		return nil, err
	}
	if c.PushPollInterval, err = envDuration("EVENTD_PUSH_POLL_INTERVAL", time.Second); err != nil {
		return nil, err
	}
	if c.ShutdownTimeout, err = envDuration("EVENTD_SHUTDOWN_TIMEOUT", 10*time.Second); err != nil {
		return nil, err
	}

	if c.PullDefaultSize <= 0 {
		c.PullDefaultSize = 100
	}
	if c.PullMaxSize < c.PullDefaultSize {
		c.PullMaxSize = c.PullDefaultSize
	}
	if c.PushBatchSize <= 0 {
		c.PushBatchSize = 50
	}
	if c.PushMaxAttempts <= 0 {
		c.PushMaxAttempts = 6
	}
	if c.IngressLogBodyMax <= 0 {
		c.IngressLogBodyMax = 8192
	}
	return c, nil
}

// resolveHTTPAddr picks the address the HTTP server listens on.
//
// Precedence, first usable value wins:
//
//  1. EVENTD_HTTP_ADDR — the service's own setting, and the only one that can
//     also move the bind host (it takes a full "host:port").
//  2. SERVICE_PORT — port only, injected by the deployment platform from the
//     service contract.
//  3. DefaultHTTPAddr.
//
// A variable that is set but unusable (not a port, out of range) falls back to
// the default rather than failing the start: the process must still come up and
// say what it ignored, so a typo costs one warning line instead of a restart
// loop whose only symptom is a health check that never passes. The warning is
// returned rather than logged — see Config.HTTPAddrWarning.
func resolveHTTPAddr() (addr, source, warning string) {
	if v := strings.TrimSpace(os.Getenv(EnvHTTPAddr)); v != "" {
		return v, EnvHTTPAddr, ""
	}

	raw := strings.TrimSpace(os.Getenv(EnvServicePort))
	if raw == "" {
		return DefaultHTTPAddr, "default", ""
	}

	// The documented shape is a bare port. A full "host:port" is tolerated
	// because it is unambiguous, and dropping it would silently move the
	// service off the port the caller asked for.
	host := ""
	if h, p, err := net.SplitHostPort(raw); err == nil {
		host, raw = h, p
	} else if strings.Contains(raw, ":") {
		return fallbackHTTPAddr(EnvServicePort, os.Getenv(EnvServicePort), err)
	}

	port, err := parsePort(raw)
	if err != nil {
		return fallbackHTTPAddr(EnvServicePort, os.Getenv(EnvServicePort), err)
	}
	return net.JoinHostPort(host, port), EnvServicePort, ""
}

// fallbackHTTPAddr yields the default address plus a warning describing the
// unusable value that was set.
func fallbackHTTPAddr(name, value string, err error) (addr, source, warning string) {
	return DefaultHTTPAddr, "default",
		fmt.Sprintf("ignoring unusable listen address %s=%q (%v), using %s",
			name, value, err, DefaultHTTPAddr)
}

// parsePort validates a port-as-string and returns its canonical decimal form.
func parsePort(v string) (string, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return "", fmt.Errorf("not a port number: %q", v)
	}
	if n < 1 || n > 65535 {
		return "", fmt.Errorf("port out of range 1..65535: %d", n)
	}
	return strconv.Itoa(n), nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s: %w", key, err)
	}
	return d, nil
}
