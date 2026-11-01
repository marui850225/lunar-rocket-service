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

var (
	// ErrInvalid wraps every message validation failure.
	ErrInvalid = errors.New("invalid message")
	// ErrNotFound means the rocket is unknown or has not launched yet.
	ErrNotFound = errors.New("rocket not found")
	// ErrInvalidSort wraps an unknown sort field or order.
	ErrInvalidSort = errors.New("invalid sort")
)

// Service implements the use cases on top of a storage.Storage.
type Service struct {
	store storage.Storage
	log   *slog.Logger
}

// New returns a Service.
func New(store storage.Storage, log *slog.Logger) *Service {
	return &Service{store: store, log: log}
}
