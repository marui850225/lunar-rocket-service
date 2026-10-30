// Package storage defines what the service needs from persistent storage.
// It holds no SQL and no business rules: implementations (see storage/sqlite)
// read and write rows; the service decides what to read and write.
package storage

import (
	"context"

	"rockets-service/internal/model"
)

// Storage is the persistent store of rockets and their buffered messages.
type Storage interface {
	// InChannel runs fn in one transaction that no other InChannel call for
	// the same channel can interleave with. If fn returns an error, nothing
	// it wrote is kept.
	InChannel(ctx context.Context, channel string, fn func(Tx) error) error

	// GetRocket returns a rocket's state; found is false if the channel has
	// no saved state.
	GetRocket(ctx context.Context, channel string) (r model.Rocket, found bool, err error)

	// ListRockets returns rockets ordered as opts says.
	ListRockets(ctx context.Context, opts ListOptions) ([]model.Rocket, error)

	// Channels describes every known channel, including ones that only have
	// buffered messages. For debugging.
	Channels(ctx context.Context) ([]ChannelInfo, error)

	Close() error
}

// Tx operates on one channel inside an InChannel transaction.
type Tx interface {
	// Rocket returns the channel's state, or a zero Rocket if none is saved.
	Rocket() (model.Rocket, error)
	SaveRocket(model.Rocket) error

	// AddPending buffers a message. added is false if a message with the
	// same number is already buffered.
	AddPending(model.Message) (added bool, err error)
	// Pending returns the buffered message with this number, if any.
	Pending(number int) (m model.Message, found bool, err error)
	// DeletePending removes buffered messages numbered from..to, inclusive.
	DeletePending(from, to int) error
	CountPending() (int, error)
}

// SortField names a rocket field to order a list by.
type SortField string

const (
	SortID         SortField = "id"
	SortType       SortField = "type"
	SortSpeed      SortField = "speed"
	SortMission    SortField = "mission"
	SortStatus     SortField = "status"
	SortLaunchedAt SortField = "launchedAt"
	SortUpdatedAt  SortField = "updatedAt"
)

// SortFields lists every SortField.
var SortFields = []SortField{SortID, SortType, SortSpeed, SortMission, SortStatus, SortLaunchedAt, SortUpdatedAt}

// ListOptions controls ListRockets. Ties are always broken by id ascending.
type ListOptions struct {
	SortBy       SortField
	Desc         bool
	LaunchedOnly bool
}

// ChannelInfo is the bookkeeping of one channel, for debugging.
type ChannelInfo struct {
	Channel           string `json:"channel"`
	Launched          bool   `json:"launched"`
	LastMessageNumber int    `json:"lastMessageNumber"`
	PendingCount      int    `json:"pendingCount"`
	// PendingNumbers lists the lowest buffered message numbers, at most 100.
	PendingNumbers []int `json:"pendingNumbers"`
}
