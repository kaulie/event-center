package config

import "testing"

// The listen address has three sources and the order matters: the deployment
// platform injects SERVICE_PORT into every start command, but an operator
// pinning EVENTD_HTTP_ADDR (systemd EnvironmentFile, docker run -e) must still
// win, and a bare `go run` must not change behaviour at all.
func TestResolveHTTPAddr(t *testing.T) {
	tests := []struct {
		name        string
		httpAddr    string
		servicePort string
		wantAddr    string
		wantSource  string
		wantWarning bool // the value was set but unusable, so it was reported
	}{
		{
			name:       "no variables falls back to the default",
			wantAddr:   DefaultHTTPAddr,
			wantSource: "default",
		},
		{
			name:        "SERVICE_PORT is honoured",
			servicePort: "9099",
			wantAddr:    ":9099",
			wantSource:  EnvServicePort,
		},
		{
			name:        "SERVICE_PORT may carry an explicit host",
			servicePort: "127.0.0.1:9099",
			wantAddr:    "127.0.0.1:9099",
			wantSource:  EnvServicePort,
		},
		{
			name:        "leading zeros are normalised away",
			servicePort: "09099",
			wantAddr:    ":9099",
			wantSource:  EnvServicePort,
		},
		{
			name:        "surrounding whitespace is tolerated",
			servicePort: " 9099 ",
			wantAddr:    ":9099",
			wantSource:  EnvServicePort,
		},
		{
			name:        "EVENTD_HTTP_ADDR wins over SERVICE_PORT",
			httpAddr:    "0.0.0.0:8081",
			servicePort: "9099",
			wantAddr:    "0.0.0.0:8081",
			wantSource:  EnvHTTPAddr,
		},
		{
			name:        "blank EVENTD_HTTP_ADDR does not shadow SERVICE_PORT",
			httpAddr:    "   ",
			servicePort: "9099",
			wantAddr:    ":9099",
			wantSource:  EnvServicePort,
		},
		{
			name:        "blank SERVICE_PORT falls back to the default",
			servicePort: "",
			wantAddr:    DefaultHTTPAddr,
			wantSource:  "default",
		},
		{
			name:        "non-numeric SERVICE_PORT falls back to the default",
			servicePort: "http",
			wantAddr:    DefaultHTTPAddr,
			wantSource:  "default",
			wantWarning: true,
		},
		{
			name:        "zero SERVICE_PORT falls back to the default",
			servicePort: "0",
			wantAddr:    DefaultHTTPAddr,
			wantSource:  "default",
			wantWarning: true,
		},
		{
			name:        "out-of-range SERVICE_PORT falls back to the default",
			servicePort: "70000",
			wantAddr:    DefaultHTTPAddr,
			wantSource:  "default",
			wantWarning: true,
		},
		{
			name:        "negative SERVICE_PORT falls back to the default",
			servicePort: "-1",
			wantAddr:    DefaultHTTPAddr,
			wantSource:  "default",
			wantWarning: true,
		},
		{
			name:        "SERVICE_PORT with a non-numeric port falls back to the default",
			servicePort: "127.0.0.1:http",
			wantAddr:    DefaultHTTPAddr,
			wantSource:  "default",
			wantWarning: true,
		},
		{
			name:        "host without a port falls back to the default",
			servicePort: "127.0.0.1",
			wantAddr:    DefaultHTTPAddr,
			wantSource:  "default",
			wantWarning: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// t.Setenv registers the restore even when the value is empty, so
			// each case starts from a clean environment.
			t.Setenv(EnvHTTPAddr, tc.httpAddr)
			t.Setenv(EnvServicePort, tc.servicePort)

			gotAddr, gotSource, gotWarning := resolveHTTPAddr()
			if gotAddr != tc.wantAddr || gotSource != tc.wantSource {
				t.Fatalf("resolveHTTPAddr() = (%q, %q, %q), want (%q, %q, warning=%v)",
					gotAddr, gotSource, gotWarning, tc.wantAddr, tc.wantSource, tc.wantWarning)
			}
			if (gotWarning != "") != tc.wantWarning {
				t.Fatalf("resolveHTTPAddr() warning = %q, wantWarning = %v",
					gotWarning, tc.wantWarning)
			}
		})
	}
}

func TestLoadUsesServicePort(t *testing.T) {
	t.Setenv(EnvHTTPAddr, "")
	t.Setenv(EnvServicePort, "9099")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":9099" || cfg.HTTPAddrSource != EnvServicePort {
		t.Fatalf("cfg.HTTPAddr/source = %q/%q, want :9099/%s",
			cfg.HTTPAddr, cfg.HTTPAddrSource, EnvServicePort)
	}
	if cfg.HTTPAddrWarning != "" {
		t.Fatalf("cfg.HTTPAddrWarning = %q, want empty", cfg.HTTPAddrWarning)
	}
}

func TestLoadDefaultsHTTPAddr(t *testing.T) {
	t.Setenv(EnvHTTPAddr, "")
	t.Setenv(EnvServicePort, "not-a-port")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != DefaultHTTPAddr || cfg.HTTPAddrSource != "default" {
		t.Fatalf("cfg.HTTPAddr/source = %q/%q, want %s/default",
			cfg.HTTPAddr, cfg.HTTPAddrSource, DefaultHTTPAddr)
	}
	// The bad value must not be swallowed silently: it is reported so the
	// caller can log it through the real (JSON) logger.
	if cfg.HTTPAddrWarning == "" {
		t.Fatal("cfg.HTTPAddrWarning is empty, want the ignored value to be reported")
	}
}
