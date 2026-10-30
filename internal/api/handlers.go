// Package api is the HTTP layer: it decodes requests, calls the store, and
// maps outcomes to status codes. It holds no rocket state of its own.
package api

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"rockets-service/internal/rocket"
	"rockets-service/internal/store"
)

// maxBodyBytes bounds a POST /messages body; real messages are ~300 bytes.
const maxBodyBytes = 64 << 10

// Store is what the handlers need from the store.
type Store interface {
	Ingest(rocket.Message) (store.Result, error)
	Get(channel string) (rocket.Rocket, bool)
	List() []rocket.Rocket
	Debug() []store.ChannelInfo
}

type handler struct {
	store Store
	log   *slog.Logger
}

// New returns the service's HTTP handler.
func New(s Store, log *slog.Logger) http.Handler {
	h := &handler{store: s, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", h.postMessage)
	mux.HandleFunc("GET /rockets", h.listRockets)
	mux.HandleFunc("GET /rockets/{channel}", h.getRocket)
	// Read-only view of internal bookkeeping (watermark, pending buffer) for
	// debugging and demos. Would sit behind auth or an admin port in production.
	mux.HandleFunc("GET /debug/store", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.Debug())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, "ok\n")
	})
	return mux
}

func (h *handler) postMessage(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading body: "+err.Error())
		return
	}
	msg, err := rocket.Decode(body)
	if err != nil {
		h.log.Warn("invalid message rejected", "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := h.store.Ingest(msg)
	switch {
	case errors.Is(err, store.ErrBufferFull):
		h.log.Warn("pending buffer full, asking for redelivery", "channel", msg.Channel, "number", msg.Number)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		h.log.Error("ingest failed", "channel", msg.Channel, "number", msg.Number, "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.log.Debug("message ingested", "channel", msg.Channel, "number", msg.Number, "type", msg.Type, "result", res)
	writeJSON(w, http.StatusOK, map[string]string{"result": res.String()})
}

func (h *handler) getRocket(w http.ResponseWriter, r *http.Request) {
	rk, ok := h.store.Get(r.PathValue("channel"))
	if !ok {
		writeError(w, http.StatusNotFound, "rocket not found")
		return
	}
	writeJSON(w, http.StatusOK, toJSON(rk))
}

func (h *handler) listRockets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sortBy := cmp.Or(q.Get("sort"), "launchedAt")
	order := cmp.Or(q.Get("order"), "asc")

	compare, ok := sortFields[sortBy]
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown sort field %q (want one of %s)", sortBy, sortFieldNames))
		return
	}
	if order != "asc" && order != "desc" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown order %q (want asc or desc)", order))
		return
	}

	rockets := h.store.List()
	slices.SortFunc(rockets, func(a, b rocket.Rocket) int {
		c := compare(a, b)
		if order == "desc" {
			c = -c
		}
		// Ties always break by id ascending, so the order is stable across calls.
		return cmp.Or(c, strings.Compare(a.ID, b.ID))
	})

	out := make([]rocketJSON, len(rockets))
	for i, rk := range rockets {
		out[i] = toJSON(rk)
	}
	writeJSON(w, http.StatusOK, out)
}

var sortFields = map[string]func(a, b rocket.Rocket) int{
	"id":         func(a, b rocket.Rocket) int { return strings.Compare(a.ID, b.ID) },
	"type":       func(a, b rocket.Rocket) int { return strings.Compare(a.Type, b.Type) },
	"speed":      func(a, b rocket.Rocket) int { return cmp.Compare(a.Speed, b.Speed) },
	"mission":    func(a, b rocket.Rocket) int { return strings.Compare(a.Mission, b.Mission) },
	"status":     func(a, b rocket.Rocket) int { return strings.Compare(string(a.Status), string(b.Status)) },
	"launchedAt": func(a, b rocket.Rocket) int { return a.LaunchedAt.Compare(b.LaunchedAt) },
	"updatedAt":  func(a, b rocket.Rocket) int { return a.UpdatedAt.Compare(b.UpdatedAt) },
}

var sortFieldNames = func() string {
	names := make([]string, 0, len(sortFields))
	for k := range sortFields {
		names = append(names, k)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}()

type rocketJSON struct {
	ID                string        `json:"id"`
	Type              string        `json:"type"`
	Speed             int64         `json:"speed"`
	Mission           string        `json:"mission"`
	Status            rocket.Status `json:"status"`
	ExplosionReason   *string       `json:"explosionReason"`
	LaunchedAt        time.Time     `json:"launchedAt"`
	UpdatedAt         time.Time     `json:"updatedAt"`
	LastMessageNumber int           `json:"lastMessageNumber"`
}

func toJSON(r rocket.Rocket) rocketJSON {
	out := rocketJSON{
		ID: r.ID, Type: r.Type, Speed: r.Speed, Mission: r.Mission, Status: r.Status,
		LaunchedAt: r.LaunchedAt, UpdatedAt: r.UpdatedAt, LastMessageNumber: r.LastMessageNumber,
	}
	if r.Status == rocket.StatusExploded {
		out.ExplosionReason = &r.ExplosionReason
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
