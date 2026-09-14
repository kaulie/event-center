package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kaulie/event-center/internal/api"
)

// syncBuffer is a goroutine-safe log sink for assertions.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestIngressLogRecordsAcceptedEventWithCorrelation(t *testing.T) {
	sink := &syncBuffer{}
	ts, _, _ := newTestServerFull(t, "", sink, "")
	body := `{"repository":{"full_name":"kaulie/event-center"}}`

	resp, payload := githubDelivery(t, ts, "push", "corr-123", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, payload)
	}
	// The delivery id is echoed back so callers can quote it.
	if got := resp.Header.Get("X-Request-ID"); got != "corr-123" {
		t.Fatalf("X-Request-ID = %q, want corr-123", got)
	}

	var out struct {
		Event struct {
			ID  string `json:"id"`
			Seq int64  `json:"seq"`
		} `json:"event"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	logs := sink.String()
	for _, want := range []string{
		"ingress accepted",
		"outcome=accepted",
		"request_id=corr-123",
		"source=github",
		"event_id=" + out.Event.ID,
		"provider=github",
		"type=github.push",
		"body_bytes=",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("ingress log missing %q, got:\n%s", want, logs)
		}
	}
	// Accepted payloads are already stored, so the body must not be duplicated
	// into the log by default.
	if strings.Contains(logs, "body=") {
		t.Fatalf("accepted request body should not be logged by default:\n%s", logs)
	}
}

func TestIngressLogRecordsRejectionsWithReasonAndBody(t *testing.T) {
	sink := &syncBuffer{}
	ts, _, _ := newTestServerFull(t, "", sink, "")

	// Bad signature: nothing is persisted anywhere, so the body must be kept.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github",
		bytes.NewBufferString(`{"repository":{"full_name":"kaulie/x"},"action":"opened"}`))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	logs := sink.String()
	for _, want := range []string{
		"ingress rejected",
		"outcome=rejected",
		"reason=\"signature verification failed",
		`body="{\"repository\"`,
		"body_sha256=",
		"source=github",
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("rejection log missing %q, got:\n%s", want, logs)
		}
	}
}

func TestIngressLogRecordsUnknownSource(t *testing.T) {
	sink := &syncBuffer{}
	ts, _, _ := newTestServerFull(t, "", sink, "")

	resp, err := http.Post(ts.URL+"/v1/ingest/nope", "application/json", bytes.NewBufferString(`{"type":"x"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	logs := sink.String()
	if !strings.Contains(logs, "reason=\"unknown source nope\"") {
		t.Fatalf("expected the rejection reason to be logged, got:\n%s", logs)
	}
	if !strings.Contains(logs, "outcome=rejected") {
		t.Fatalf("expected outcome=rejected, got:\n%s", logs)
	}
}

func TestIngressAuditFileIsAppendOnlyJSONL(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "ingress.log")
	sink := &syncBuffer{}
	ts, _, _ := newTestServerFull(t, "", sink, auditPath)

	githubDelivery(t, ts, "push", "d-audit-1", `{"repository":{"full_name":"kaulie/event-center"}}`)
	// duplicate
	githubDelivery(t, ts, "push", "d-audit-1", `{"repository":{"full_name":"kaulie/event-center"}}`)

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit file has %d lines, want 2:\n%s", len(lines), raw)
	}

	first := map[string]any{}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("audit line is not JSON: %v (%s)", err, lines[0])
	}
	if first["outcome"] != "accepted" {
		t.Fatalf("first outcome = %v, want accepted", first["outcome"])
	}
	if first["event_id"] == "" || first["event_id"] == nil {
		t.Fatalf("audit line must carry the event id: %v", first)
	}

	second := map[string]any{}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("audit line is not JSON: %v", err)
	}
	if second["outcome"] != "duplicate" {
		t.Fatalf("second outcome = %v, want duplicate", second["outcome"])
	}
}

func TestIngressLogBodyCanBeEnabledForAcceptedRequests(t *testing.T) {
	sink := &syncBuffer{}
	ts, _, _ := newTestServerOpts(t, "", sink, "", func(c *api.Config) {
		c.IngressLogBody = true
	})

	githubDelivery(t, ts, "push", "d-body", `{"repository":{"full_name":"kaulie/event-center"}}`)

	logs := sink.String()
	if !strings.Contains(logs, "body=") {
		t.Fatalf("expected the body to be logged when enabled, got:\n%s", logs)
	}
	if !strings.Contains(logs, "body_truncated=false") {
		t.Fatalf("expected body_truncated marker, got:\n%s", logs)
	}
}

func TestIngressLogTruncatesHugeBodiesOnRejection(t *testing.T) {
	sink := &syncBuffer{}
	ts, _, _ := newTestServerOpts(t, "", sink, "", func(c *api.Config) {
		c.IngressLogBodyMax = 64
	})

	huge := `{"repository":{"full_name":"kaulie/x"},"pad":"` + strings.Repeat("z", 500) + `"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewBufferString(huge))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=bad")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()

	logs := sink.String()
	if !strings.Contains(logs, "body_truncated=true") {
		t.Fatalf("expected the captured body to be flagged as truncated:\n%s", logs)
	}
	if strings.Contains(logs, strings.Repeat("z", 200)) {
		t.Fatalf("body was not truncated to IngressLogBodyMax:\n%s", logs)
	}
}
