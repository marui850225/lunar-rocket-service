// Package api is the HTTP layer: it reads requests, calls the service, and
// maps outcomes to status codes and JSON. It holds no business rules.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"rockets-service/internal/model"
	"rockets-service/internal/service"
)

// maxBodyBytes bounds a POST /messages body; real messages are ~300 bytes.
const maxBodyBytes = 64 << 10

type handler struct {
	svc *service.Service
	log *slog.Logger
}

// New returns the service's HTTP handler.
func New(svc *service.Service, log *slog.Logger) http.Handler {
	h := &handler{svc: svc, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", h.postMessage)
	mux.HandleFunc("GET /rockets", h.listRockets)
	mux.HandleFunc("GET /rockets/{channel}", h.getRocket)
	// Read-only view of each channel's progress and buffer, for debugging.
	// Would need auth or an admin port in production.
	mux.HandleFunc("GET /debug/store", h.debug)
	return mux
}

func (h *handler) postMessage(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading body: "+err.Error())
		return
	}

	res, err := h.svc.Ingest(r.Context(), body)
	switch {
	case errors.Is(err, service.ErrInvalid):
		h.log.Warn("invalid message rejected", "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		h.internalError(w, "ingest failed", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"result": string(res)})
	}
}

func (h *handler) getRocket(w http.ResponseWriter, r *http.Request) {
	rk, err := h.svc.GetRocket(r.Context(), r.PathValue("channel"))
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		h.internalError(w, "get rocket failed", err)
	default:
		writeJSON(w, http.StatusOK, toJSON(rk))
	}
}

func (h *handler) listRockets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rockets, err := h.svc.ListRockets(r.Context(), q.Get("sort"), q.Get("order"))
	switch {
	case errors.Is(err, service.ErrInvalidSort):
		writeError(w, http.StatusBadRequest, err.Error())
	case err != nil:
		h.internalError(w, "list rockets failed", err)
	default:
		out := make([]rocketJSON, len(rockets))
		for i, rk := range rockets {
			out[i] = toJSON(rk)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (h *handler) debug(w http.ResponseWriter, r *http.Request) {
	infos, err := h.svc.Debug(r.Context())
	if err != nil {
		h.internalError(w, "debug failed", err)
		return
	}
	writeJSON(w, http.StatusOK, infos)
}

// internalError logs the cause and returns a generic 500, so internal
// details never reach the client.
func (h *handler) internalError(w http.ResponseWriter, msg string, err error) {
	h.log.Error(msg, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

type rocketJSON struct {
	ID                string       `json:"id"`
	Type              string       `json:"type"`
	Speed             int64        `json:"speed"`
	Mission           string       `json:"mission"`
	Status            model.Status `json:"status"`
	ExplosionReason   *string      `json:"explosionReason"`
	LaunchedAt        time.Time    `json:"launchedAt"`
	UpdatedAt         time.Time    `json:"updatedAt"`
	LastMessageNumber int          `json:"lastMessageNumber"`
}

func toJSON(r model.Rocket) rocketJSON {
	out := rocketJSON{
		ID: r.ID, Type: r.Type, Speed: r.Speed, Mission: r.Mission, Status: r.Status,
		LaunchedAt: r.LaunchedAt, UpdatedAt: r.UpdatedAt, LastMessageNumber: r.LastMessageNumber,
	}
	if r.Status == model.StatusExploded {
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
