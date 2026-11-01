// Package storage defines what the service needs from persistent storage.
// It holds no SQL and no business rules: implementations (see storage/sqlite)
// read and write rows; the service decides what to read and write.
//
// Storage only runs transactions. All data access goes through the two
// table interfaces, which exist only inside a transaction.
package storage

import (
	"context"

	"rockets-service/internal/model"
)

// Storage runs transactions over the rockets and pending tables.
type Storage interface {
	// InTx runs fn in one transaction, giving it both tables. Concurrent
	// InTx calls cannot interleave with it. If fn returns an error, nothing
	// it wrote is kept.
	InTx(ctx context.Context, fn func(Tables) error) error

	Close() error
}

// Tables gives InTx's function both tables, bound to that transaction.
type Tables struct {
	Rockets RocketTable
	Pending PendingTable
}

// RocketTable is the rockets table: one row per channel with the rocket's
// current state.
type RocketTable interface {
	// Get returns a channel's rocket; found is false if none is saved.
	Get(channel string) (r model.Rocket, found bool, err error)
	// Save inserts or updates the rocket, keyed by its ID (the channel).
	Save(model.Rocket) error
	// List returns rockets ordered as opts says.
	List(opts ListOptions) ([]model.Rocket, error)
}

// PendingTable is the pending table: messages that arrived before the ones
// they follow, waiting for the gap to fill.
type PendingTable interface {
	// Add buffers a message. added is false if one with the same channel and
	// number is already buffered.
	Add(model.Message) (added bool, err error)
	// Get returns the buffered message with this channel and number, if any.
	Get(channel string, number int) (m model.Message, found bool, err error)
	// Delete removes a channel's buffered messages numbered from..to, inclusive.
	Delete(channel string, from, to int) error
	// Summary describes every channel's progress and buffer, including
	// channels that only have buffered messages. For debugging.
	Summary() ([]model.ChannelInfo, error)
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

// ListOptions controls RocketTable.List. Ties are always broken by id ascending.
type ListOptions struct {
	SortBy       SortField
	Desc         bool
	LaunchedOnly bool
}
