// Package service holds the business logic: decoding and validating
// messages, the rules for applying them, ordering and deduplication through
// the pending buffer, and the rules for reading rockets back. It talks to
// storage only through the storage.Storage interface.
package service

import (
	"errors"
	"log/slog"

	"rockets-service/internal/storage"
)

// DefaultMaxPending caps how many early messages one rocket may buffer.
const DefaultMaxPending = 10_000

var (
	// ErrInvalid wraps every message validation failure.
	ErrInvalid = errors.New("invalid message")
	// ErrBufferFull means a message arrived early and its rocket's buffer is
	// full; the sender should redeliver it later.
	ErrBufferFull = errors.New("pending buffer full")
	// ErrNotFound means the rocket is unknown or has not launched yet.
	ErrNotFound = errors.New("rocket not found")
	// ErrInvalidSort wraps an unknown sort field or order.
	ErrInvalidSort = errors.New("invalid sort")
)

// Service implements the use cases on top of a storage.Storage.
type Service struct {
	store      storage.Storage
	log        *slog.Logger
	maxPending int
}

// New returns a Service. maxPending <= 0 means DefaultMaxPending.
func New(store storage.Storage, log *slog.Logger, maxPending int) *Service {
	if maxPending <= 0 {
		maxPending = DefaultMaxPending
	}
	return &Service{store: store, log: log, maxPending: maxPending}
}
