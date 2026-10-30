// Package api is the HTTP layer: it reads requests, calls the service, and
// maps outcomes to status codes and JSON. It holds no business rules.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"rockets-service/internal/model"
	"rockets-service/internal/service"
	"rockets-service/internal/storage"
)

// maxBodyBytes bounds a POST /messages body; real messages are ~300 bytes.
const maxBodyBytes = 64 << 10

// Service is what the handlers need from the service layer.
type Service interface {
	Ingest(ctx context.Context, body []byte) (service.Result, error)
	GetRocket(ctx context.Context, id string) (model.Rocket, error)
	ListRockets(ctx context.Context, sortBy, order string) ([]model.Rocket, error)
	Debug(ctx context.Context) ([]storage.ChannelInfo, error)
}

type handler struct {
	svc Service
	log *slog.Logger
}

// New returns the service's HTTP handler.
func New(svc Service, log *slog.Logger) http.Handler {
	h := &handler{svc: svc, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /messages", h.postMessage)
	mux.HandleFunc("GET /rockets", h.listRockets)
	mux.HandleFunc("GET /rockets/{channel}", h.getRocket)
	// Read-only view of internal bookkeeping (watermark, pending buffer) for
	// debugging and demos. Would sit behind auth or an admin port in production.
	mux.HandleFunc("GET /debug/store", h.debug)
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

	res, err := h.svc.Ingest(r.Context(), body)
	switch {
	case errors.Is(err, service.ErrInvalid):
		h.log.Warn("invalid message rejected", "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrBufferFull):
		h.log.Warn("pending buffer full, asking for redelivery", "err", err)
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		h.internalError(w, r, "ingest failed", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"result": res.String()})
	}
}

func (h *handler) getRocket(w http.ResponseWriter, r *http.Request) {
	rk, err := h.svc.GetRocket(r.Context(), r.PathValue("channel"))
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		h.internalError(w, r, "get rocket failed", err)
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
		h.internalError(w, r, "list rockets failed", err)
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
		h.internalError(w, r, "debug failed", err)
		return
	}
	writeJSON(w, http.StatusOK, infos)
}

// internalError logs the cause and returns a generic 500, so internal
// details never reach the client. A request whose client went away (or
// timed out) while it waited is not a server fault: it is logged quietly
// and answered 503, which a still-listening sender may retry.
func (h *handler) internalError(w http.ResponseWriter, r *http.Request, msg string, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		h.log.Info(msg+": request cancelled", "err", err)
		writeError(w, http.StatusServiceUnavailable, "request cancelled")
		return
	}
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
