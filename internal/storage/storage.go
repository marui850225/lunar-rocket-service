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
	// InChannel runs fn in one transaction, giving it this channel's rows in
	// both tables. No other InChannel call for the same channel can
	// interleave with it. If fn returns an error, nothing it wrote is kept.
	InChannel(ctx context.Context, channel string, fn func(Tables) error) error

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

// Tables gives InChannel's function one channel's rows in each table. Every
// call runs inside that InChannel transaction.
type Tables struct {
	Rocket  RocketTable
	Pending PendingTable
}

// RocketTable is the channel's row in the rockets table: its current state.
type RocketTable interface {
	// Get returns the rocket, or a zero Rocket if none is saved yet.
	Get() (model.Rocket, error)
	// Save inserts or updates the rocket.
	Save(model.Rocket) error
}

// PendingTable is the channel's rows in the pending table: messages that
// arrived before the ones they follow.
type PendingTable interface {
	// Add buffers a message. added is false if one with the same number is
	// already buffered.
	Add(model.Message) (added bool, err error)
	// Get returns the buffered message with this number, if any.
	Get(number int) (m model.Message, found bool, err error)
	// Delete removes buffered messages numbered from..to, inclusive.
	Delete(from, to int) error
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
