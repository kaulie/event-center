// Package config loads runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	HTTPAddr        string
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

	ShutdownTimeout time.Duration
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	c := &Config{
		HTTPAddr:           env("EVENTD_HTTP_ADDR", ":8080"),
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
	}

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
	return c, nil
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
