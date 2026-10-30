package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"rockets-service/internal/rocket"
	"rockets-service/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newServer(t *testing.T, maxPending int) http.Handler {
	t.Helper()
	return New(store.New(quiet, maxPending), quiet)
}

func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func message(channel string, n int, msgType, payload string) string {
	return fmt.Sprintf(`{"metadata":{"channel":%q,"messageNumber":%d,"messageTime":"2022-02-02T19:39:%02d+01:00","messageType":%q},"message":%s}`,
		channel, n, n%60, msgType, payload)
}

func launched(channel, rocketType string, speed int) string {
	return message(channel, 1, "RocketLaunched", fmt.Sprintf(`{"type":%q,"launchSpeed":%d,"mission":"ARTEMIS"}`, rocketType, speed))
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestPostMessageStatusCodes(t *testing.T) {
	h := newServer(t, 1)
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantResult string
	}{
		{"buffered before launch", message("a", 2, "RocketSpeedIncreased", `{"by":100}`), 200, "buffered"},
		{"applied, drains buffer", launched("a", "Falcon-9", 500), 200, "applied"},
		{"duplicate", launched("a", "Falcon-9", 500), 200, "duplicate"},
		{"early message fills buffer", message("a", 4, "RocketSpeedIncreased", `{"by":1}`), 200, "buffered"},
		{"buffer full", message("a", 5, "RocketSpeedIncreased", `{"by":1}`), 503, ""},
		{"malformed JSON", `{nope`, 400, ""},
		{"unknown type", message("a", 6, "RocketLanded", `{}`), 400, ""},
		{"missing field", message("a", 6, "RocketSpeedIncreased", `{}`), 400, ""},
	}
	for _, tt := range tests {
		rec := do(t, h, "POST", "/messages", tt.body)
		if rec.Code != tt.wantStatus {
			t.Fatalf("%s: status %d, want %d (body %s)", tt.name, rec.Code, tt.wantStatus, rec.Body)
		}
		got := decode[map[string]string](t, rec)
		if tt.wantStatus == 200 && got["result"] != tt.wantResult {
			t.Fatalf("%s: result %q, want %q", tt.name, got["result"], tt.wantResult)
		}
		if tt.wantStatus != 200 && got["error"] == "" {
			t.Fatalf("%s: missing error message in %s", tt.name, rec.Body)
		}
		if tt.wantStatus == 503 && rec.Header().Get("Retry-After") == "" {
			t.Fatalf("%s: missing Retry-After", tt.name)
		}
	}
}

type failingStore struct{ Store }

func (failingStore) Ingest(rocket.Message) (store.Result, error) {
	return 0, errors.New("disk on fire")
}

func TestPostMessageInternalError(t *testing.T) {
	h := New(failingStore{}, quiet)
	rec := do(t, h, "POST", "/messages", launched("a", "Falcon-9", 500))
	if rec.Code != 500 {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "disk on fire") {
		t.Fatalf("internal error leaked to client: %s", body)
	}
}

func TestGetRocket(t *testing.T) {
	h := newServer(t, 0)
	do(t, h, "POST", "/messages", launched("a", "Falcon-9", 500))
	do(t, h, "POST", "/messages", message("a", 2, "RocketSpeedIncreased", `{"by":3000}`))
	do(t, h, "POST", "/messages", message("b", 2, "RocketSpeedIncreased", `{"by":1}`)) // not launched

	rec := do(t, h, "GET", "/rockets/a", "")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	want := `{"id":"a","type":"Falcon-9","speed":3500,"mission":"ARTEMIS","status":"ACTIVE","explosionReason":null,` +
		`"launchedAt":"2022-02-02T19:39:01+01:00","updatedAt":"2022-02-02T19:39:02+01:00","lastMessageNumber":2}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body\n got  %s\n want %s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}

	for _, id := range []string{"b", "unknown"} {
		if rec := do(t, h, "GET", "/rockets/"+id, ""); rec.Code != 404 {
			t.Fatalf("GET %s: status %d, want 404", id, rec.Code)
		}
	}
}

func TestExplodedRocketHasReason(t *testing.T) {
	h := newServer(t, 0)
	do(t, h, "POST", "/messages", launched("a", "Falcon-9", 500))
	do(t, h, "POST", "/messages", message("a", 2, "RocketExploded", `{"reason":"PRESSURE_VESSEL_FAILURE"}`))
	got := decode[map[string]any](t, do(t, h, "GET", "/rockets/a", ""))
	if got["status"] != "EXPLODED" || got["explosionReason"] != "PRESSURE_VESSEL_FAILURE" {
		t.Fatalf("got %v", got)
	}
}

func TestListRockets(t *testing.T) {
	h := newServer(t, 0)
	// c launches first, then a, then b (launch times differ by message time).
	do(t, h, "POST", "/messages", strings.Replace(launched("c", "Atlas", 300), "19:39:01", "19:39:00", 1))
	do(t, h, "POST", "/messages", launched("a", "Falcon-9", 500))
	do(t, h, "POST", "/messages", strings.Replace(launched("b", "Atlas", 500), "19:39:01", "19:39:09", 1))
	do(t, h, "POST", "/messages", message("z", 2, "RocketSpeedIncreased", `{"by":1}`)) // not launched: hidden

	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"c", "a", "b"}}, // default: launchedAt asc
		{"?sort=speed", []string{"c", "a", "b"}},
		{"?sort=speed&order=desc", []string{"a", "b", "c"}}, // a,b tie on speed: id asc
		{"?sort=type", []string{"b", "c", "a"}},             // Atlas,Atlas tie: id asc
		{"?sort=id&order=desc", []string{"c", "b", "a"}},
		{"?sort=launchedAt&order=desc", []string{"b", "a", "c"}},
	}
	for _, tt := range tests {
		rec := do(t, h, "GET", "/rockets"+tt.query, "")
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", tt.query, rec.Code)
		}
		var ids []string
		for _, r := range decode[[]map[string]any](t, rec) {
			ids = append(ids, r["id"].(string))
		}
		if strings.Join(ids, ",") != strings.Join(tt.want, ",") {
			t.Fatalf("%q: got %v, want %v", tt.query, ids, tt.want)
		}
	}

	for _, q := range []string{"?sort=color", "?order=sideways"} {
		if rec := do(t, h, "GET", "/rockets"+q, ""); rec.Code != 400 {
			t.Fatalf("%s: status %d, want 400", q, rec.Code)
		}
	}
}

func TestEmptyListIsArray(t *testing.T) {
	rec := do(t, newServer(t, 0), "GET", "/rockets", "")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("body %q, want []", got)
	}
}

func TestRoutingAndHealth(t *testing.T) {
	h := newServer(t, 0)
	if rec := do(t, h, "GET", "/healthz", ""); rec.Code != 200 {
		t.Fatalf("healthz %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/messages", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /messages: %d, want 405", rec.Code)
	}
}
