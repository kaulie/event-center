package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/kaulie/event-center/internal/api"
	"github.com/kaulie/event-center/internal/metrics"
	"github.com/kaulie/event-center/internal/model"
	"github.com/kaulie/event-center/internal/service"
	"github.com/kaulie/event-center/internal/store"
	"github.com/kaulie/event-center/internal/verify"
)

const githubSecret = "github-webhook-secret"

// newTestServer wires a real store, service and router backed by a temp file.
func newTestServer(t *testing.T, adminToken string) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.UpsertSource(context.Background(), &model.Source{
		ID: "github", Kind: "webhook", Secret: githubSecret,
		VerifyMode: model.VerifyHMACSHA256, TypePrefix: "github", DefaultStream: "github", Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	if err := st.UpsertSource(context.Background(), &model.Source{
		ID: "generic", Kind: "api", Secret: "generic-token",
		VerifyMode: model.VerifyBearer, TypePrefix: "generic", DefaultStream: "generic", Enabled: true,
	}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A single registry is shared by the service and the HTTP layer, exactly
	// like in main(), so /metrics reports ingest and HTTP counters together.
	reg := metrics.New()
	svc := service.New(st, reg, log)
	srv := api.New(svc, reg, log, api.Config{
		AdminToken:      adminToken,
		PullDefaultSize: 10,
		PullMaxSize:     50,
		PullWaitMax:     time.Second,
		Version:         "test",
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func githubDelivery(t *testing.T, ts *httptest.Server, eventName, deliveryID, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", eventName)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", verify.SignHMACSHA256(githubSecret, []byte(body)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("github webhook: %v", err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	return resp, payload
}

func TestGitHubWebhookAcceptsAndTypesEvent(t *testing.T) {
	ts, _ := newTestServer(t, "")
	body := `{"action":"opened","number":7,"repository":{"full_name":"kaulie/event-center"},"sender":{"login":"gaolei"}}`

	resp, payload := githubDelivery(t, ts, "pull_request", "delivery-1", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, payload)
	}

	var out model.IngestResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, payload)
	}
	if out.Event == nil {
		t.Fatal("response must carry the stored event")
	}
	if out.Event.Type != "github.pull_request.opened" {
		t.Fatalf("type = %q, want github.pull_request.opened", out.Event.Type)
	}
	if out.Event.Provider != "github" {
		t.Fatalf("provider = %q, want github", out.Event.Provider)
	}
	if out.Event.Stream != "github" {
		t.Fatalf("stream = %q, want github", out.Event.Stream)
	}
	if out.Event.Subject != "repo:kaulie/event-center" {
		t.Fatalf("subject = %q", out.Event.Subject)
	}
	if out.Event.Seq != 1 || out.Event.StreamSeq != 1 {
		t.Fatalf("seq = %d, stream_seq = %d, want 1/1", out.Event.Seq, out.Event.StreamSeq)
	}
	if out.Event.Headers["X-GitHub-Event"] != "pull_request" {
		t.Fatalf("headers not whitelisted: %+v", out.Event.Headers)
	}
}

func TestGitHubWebhookIsIdempotentByDeliveryID(t *testing.T) {
	ts, _ := newTestServer(t, "")
	body := `{"repository":{"full_name":"kaulie/event-center"}}`

	first, _ := githubDelivery(t, ts, "push", "delivery-dup", body)
	if first.StatusCode != http.StatusAccepted {
		t.Fatalf("first status = %d", first.StatusCode)
	}
	second, payload := githubDelivery(t, ts, "push", "delivery-dup", body)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.StatusCode, payload)
	}
	var out model.IngestResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Duplicate || out.Status != "duplicate" {
		t.Fatalf("expected duplicate response, got %+v", out)
	}
}

func TestGitHubWebhookRejectsBadSignature(t *testing.T) {
	ts, _ := newTestServer(t, "")
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github",
		bytes.NewBufferString(`{"repository":{"full_name":"kaulie/x"}}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestGitHubWebhookRequiresEventHeader(t *testing.T) {
	ts, _ := newTestServer(t, "")
	body := `{"repository":{"full_name":"kaulie/x"}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Hub-Signature-256", verify.SignHMACSHA256(githubSecret, []byte(body)))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGenericIngestRequiresBearerTokenAndPrefixesType(t *testing.T) {
	ts, _ := newTestServer(t, "")
	payload := `{"type":"deploy.finished","subject":"service:api","data":{"ok":true}}`

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/ingest/generic", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/ingest/generic", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer generic-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var out model.IngestResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Event.Type != "generic.deploy.finished" {
		t.Fatalf("type = %q, want generic.deploy.finished", out.Event.Type)
	}
	if out.Event.Stream != "generic" {
		t.Fatalf("stream = %q, want generic", out.Event.Stream)
	}
}

func TestGenericIngestRejectsUnknownSource(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp, err := http.Post(ts.URL+"/v1/ingest/nope", "application/json", bytes.NewBufferString(`{"type":"x"}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestPullPaginatesByCursor(t *testing.T) {
	ts, _ := newTestServer(t, "")
	for i := 0; i < 3; i++ {
		body := `{"repository":{"full_name":"kaulie/event-center"}}`
		resp, payload := githubDelivery(t, ts, "push", "d"+string(rune('a'+i)), body)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("ingest %d status = %d, body = %s", i, resp.StatusCode, payload)
		}
	}

	page1 := getJSON[model.ListResult](t, ts.URL+"/v1/streams/github/events?after=0&limit=2", "")
	if len(page1.Events) != 2 || !page1.HasMore {
		t.Fatalf("page1 = %d events, hasMore = %v", len(page1.Events), page1.HasMore)
	}
	if page1.NextCursor != 2 {
		t.Fatalf("next_cursor = %d, want 2", page1.NextCursor)
	}

	page2 := getJSON[model.ListResult](t,
		ts.URL+"/v1/streams/github/events?after=2&limit=10", "")
	if len(page2.Events) != 1 || page2.HasMore {
		t.Fatalf("page2 = %d events, hasMore = %v", len(page2.Events), page2.HasMore)
	}
	if page2.Events[0].StreamSeq != 3 {
		t.Fatalf("page2 event stream_seq = %d, want 3", page2.Events[0].StreamSeq)
	}
}

func TestPullLongPollWaitsForNewEvent(t *testing.T) {
	ts, _ := newTestServer(t, "")

	go func() {
		time.Sleep(300 * time.Millisecond)
		githubDelivery(t, ts, "push", "delivery-live", `{"repository":{"full_name":"kaulie/live"}}`)
	}()

	start := time.Now()
	res := getJSON[model.ListResult](t, ts.URL+"/v1/streams/github/events?after=0&wait=3s", "")
	if len(res.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(res.Events))
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("long poll returned too early: %v", elapsed)
	}
	if res.Events[0].Subject != "repo:kaulie/live" {
		t.Fatalf("subject = %q", res.Events[0].Subject)
	}
}

func TestPullLongPollReturnsEmptyAfterTimeout(t *testing.T) {
	ts, _ := newTestServer(t, "")
	start := time.Now()
	res := getJSON[model.ListResult](t, ts.URL+"/v1/streams/github/events?after=0&wait=400ms", "")
	if len(res.Events) != 0 {
		t.Fatalf("got %d events, want 0", len(res.Events))
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("expected the poll to wait, returned after %v", elapsed)
	}
}

func TestAdminEndpointsRequireToken(t *testing.T) {
	ts, _ := newTestServer(t, "admin-token")

	resp, err := http.Get(ts.URL + "/v1/sources")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want 401", resp.StatusCode)
	}

	sources := getJSON[struct {
		Sources []struct {
			ID        string `json:"id"`
			HasSecret bool   `json:"has_secret"`
		} `json:"sources"`
	}](t, ts.URL+"/v1/sources", "admin-token")
	if len(sources.Sources) != 2 {
		t.Fatalf("got %d sources, want 2", len(sources.Sources))
	}
	for _, src := range sources.Sources {
		if !src.HasSecret {
			t.Fatalf("source %s should report has_secret", src.ID)
		}
	}
}

func TestCreateSubscriptionGeneratesSecretAndAckMovesCursorForward(t *testing.T) {
	ts, _ := newTestServer(t, "admin-token")

	created := postJSON[struct {
		Subscription struct {
			ID       string `json:"id"`
			Delivery string `json:"delivery"`
			Cursor   int64  `json:"cursor"`
		} `json:"subscription"`
		Secret          string `json:"secret"`
		SecretGenerated bool   `json:"secret_generated"`
	}](t, ts.URL+"/v1/subscriptions", "admin-token",
		`{"name":"ci","stream":"github","type_filters":["github.*"],"delivery":"pull"}`)

	if created.Subscription.ID == "" {
		t.Fatal("subscription id must be generated")
	}
	if created.Secret == "" || !created.SecretGenerated {
		t.Fatalf("expected a generated secret, got %+v", created)
	}

	for i := 0; i < 3; i++ {
		githubDelivery(t, ts, "push", "ack-"+string(rune('a'+i)), `{"repository":{"full_name":"kaulie/ack"}}`)
	}

	// A pull consumer authenticates with its own key.
	acked := postJSON[struct {
		Cursor int64 `json:"cursor"`
	}](t, ts.URL+"/v1/subscriptions/"+created.Subscription.ID+"/ack", created.Secret, `{"cursor":3}`)
	if acked.Cursor != 3 {
		t.Fatalf("cursor = %d, want 3", acked.Cursor)
	}

	// Rewinding is refused: the cursor stays where it was.
	rewound := postJSON[struct {
		Cursor int64 `json:"cursor"`
	}](t, ts.URL+"/v1/subscriptions/"+created.Subscription.ID+"/ack", created.Secret, `{"cursor":1}`)
	if rewound.Cursor != 3 {
		t.Fatalf("cursor = %d after rewind attempt, want 3", rewound.Cursor)
	}

	// A wrong key cannot ack.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/subscriptions/"+created.Subscription.ID+"/ack",
		bytes.NewBufferString(`{"cursor":9}`))
	req.Header.Set("X-API-Key", "not-the-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCreatePushSubscriptionRequiresEndpoint(t *testing.T) {
	ts, _ := newTestServer(t, "")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/subscriptions",
		bytes.NewBufferString(`{"name":"bad","delivery":"push"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHealthAndMetricsEndpoints(t *testing.T) {
	ts, _ := newTestServer(t, "")
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}

	githubDelivery(t, ts, "push", "metrics-1", `{"repository":{"full_name":"kaulie/m"}}`)

	resp, err = http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("eventd_events_ingested_total")) {
		t.Fatalf("metrics body missing ingest counter:\n%s", body)
	}
}

// --- helpers --------------------------------------------------------------

func getJSON[T any](t *testing.T, url, token string) T {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doJSON[T](t, req)
}

func postJSON[T any](t *testing.T, url, token, body string) T {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doJSON[T](t, req)
}

func doJSON[T any](t *testing.T, req *http.Request) T {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		t.Fatalf("%s %s returned %d: %s", req.Method, req.URL.Path, resp.StatusCode, raw)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s %s: %v (body=%s)", req.Method, req.URL.Path, err, raw)
	}
	return out
}
