package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"rockets-service/internal/service"
	"rockets-service/internal/storage/sqlite"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newServer(t *testing.T) http.Handler {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(service.New(db, quiet), quiet)
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
	h := newServer(t)
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantResult string
	}{
		{"buffered before launch", message("a", 2, "RocketSpeedIncreased", `{"by":100}`), 200, "buffered"},
		{"applied, drains buffer", launched("a", "Falcon-9", 500), 200, "applied"},
		{"duplicate", launched("a", "Falcon-9", 500), 200, "duplicate"},
		{"malformed JSON", `{nope`, 400, ""},
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
	}
}

func TestGetRocket(t *testing.T) {
	h := newServer(t)
	do(t, h, "POST", "/messages", launched("a", "Falcon-9", 500))
	do(t, h, "POST", "/messages", message("a", 2, "RocketSpeedIncreased", `{"by":3000}`))
	do(t, h, "POST", "/messages", message("b", 2, "RocketSpeedIncreased", `{"by":1}`)) // not launched

	rec := do(t, h, "GET", "/rockets/a", "")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	want := `{"id":"a","type":"Falcon-9","speed":3500,"mission":"ARTEMIS","status":"ACTIVE","explosionReason":null,` +
		`"launchedAt":"2022-02-02T18:39:01Z","updatedAt":"2022-02-02T18:39:02Z","lastMessageNumber":2}`
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
	h := newServer(t)
	do(t, h, "POST", "/messages", launched("a", "Falcon-9", 500))
	do(t, h, "POST", "/messages", message("a", 2, "RocketExploded", `{"reason":"PRESSURE_VESSEL_FAILURE"}`))
	got := decode[map[string]any](t, do(t, h, "GET", "/rockets/a", ""))
	if got["status"] != "EXPLODED" || got["explosionReason"] != "PRESSURE_VESSEL_FAILURE" {
		t.Fatalf("got %v", got)
	}
}

func TestListRockets(t *testing.T) {
	h := newServer(t)
	// c launches first, then a, then b (launch times differ by message time).
	do(t, h, "POST", "/messages", strings.Replace(launched("c", "Atlas", 300), "19:39:01", "19:39:00", 1))
	do(t, h, "POST", "/messages", launched("a", "Falcon-9", 500))
	do(t, h, "POST", "/messages", strings.Replace(launched("b", "Atlas", 500), "19:39:01", "19:39:09", 1))
	do(t, h, "POST", "/messages", message("z", 2, "RocketSpeedIncreased", `{"by":1}`)) // not launched: hidden

	tests := []struct {
		query string
		want  []string
	}{
		{"", []string{"c", "a", "b"}},                       // default: launchedAt asc
		{"?sort=speed&order=desc", []string{"a", "b", "c"}}, // a,b tie on speed: id asc
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
	rec := do(t, newServer(t), "GET", "/rockets", "")
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("body %q, want []", got)
	}
}

func TestWrongMethod(t *testing.T) {
	h := newServer(t)
	if rec := do(t, h, "GET", "/messages", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /messages: %d, want 405", rec.Code)
	}
}

func TestDebugStore(t *testing.T) {
	h := newServer(t)
	do(t, h, "POST", "/messages", message("a", 3, "RocketSpeedIncreased", `{"by":1}`))
	do(t, h, "POST", "/messages", launched("b", "Atlas", 1))
	rec := do(t, h, "GET", "/debug/store", "")
	want := `[{"channel":"a","launched":false,"lastMessageNumber":0,"pendingCount":1,"pendingNumbers":[3]},` +
		`{"channel":"b","launched":true,"lastMessageNumber":1,"pendingCount":0,"pendingNumbers":[]}]`
	if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != want {
		t.Fatalf("status %d body\n got  %s\n want %s", rec.Code, got, want)
	}
}
