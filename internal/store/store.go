// Package store applies rocket messages in order, exactly once, and safely
// under concurrent requests. It keeps all state in memory.
//
// Per channel it tracks a watermark (applied: every message up to this number
// has been applied) and a pending map of messages that arrived early. A
// message at or below the watermark, or already pending, is a duplicate.
package store

import (
	"errors"
	"log/slog"
	"sync"

	"rockets-service/internal/rocket"
)

// DefaultMaxPending caps how many early messages one rocket may buffer.
const DefaultMaxPending = 10_000

// ErrBufferFull means the message arrived early and the rocket's pending
// buffer is full. The caller should ask the sender to redeliver later.
var ErrBufferFull = errors.New("pending buffer full")

// Result says what Ingest did with an accepted message.
type Result int

const (
	Applied   Result = iota // applied now (possibly with earlier-buffered successors)
	Buffered                // stored until the gap before it fills
	Duplicate               // already applied or already pending; ignored
)

func (r Result) String() string {
	switch r {
	case Applied:
		return "applied"
	case Buffered:
		return "buffered"
	case Duplicate:
		return "duplicate"
	}
	return "unknown"
}

// Store holds every rocket. Lock order: Store.mu before entry.mu, never the reverse.
type Store struct {
	mu         sync.RWMutex
	rockets    map[string]*entry // key: channel
	maxPending int
	log        *slog.Logger
}

type entry struct {
	mu      sync.Mutex
	applied int                    // highest message number applied with no gap below it
	pending map[int]rocket.Message // early messages, keyed by number; all > applied+1
	state   rocket.Rocket
}

// New returns an empty store. maxPending <= 0 means DefaultMaxPending.
func New(log *slog.Logger, maxPending int) *Store {
	if maxPending <= 0 {
		maxPending = DefaultMaxPending
	}
	return &Store{rockets: make(map[string]*entry), maxPending: maxPending, log: log}
}

// Ingest accepts one validated message. It returns ErrBufferFull when the
// message cannot be held yet; every other outcome is a Result.
func (s *Store) Ingest(m rocket.Message) (Result, error) {
	e := s.entryFor(m.Channel)
	e.mu.Lock()
	defer e.mu.Unlock()

	if m.Number <= e.applied {
		return Duplicate, nil
	}
	if _, ok := e.pending[m.Number]; ok {
		return Duplicate, nil
	}
	// The gap-filling message is always accepted, so a full buffer can drain.
	if m.Number != e.applied+1 && len(e.pending) >= s.maxPending {
		return 0, ErrBufferFull
	}

	e.pending[m.Number] = m
	s.drain(e)

	if m.Number <= e.applied {
		return Applied, nil
	}
	return Buffered, nil
}

// drain applies pending messages for as long as the next one is present.
// The caller holds e.mu.
func (s *Store) drain(e *entry) {
	for {
		next, ok := e.pending[e.applied+1]
		if !ok {
			return
		}
		delete(e.pending, next.Number)

		before := e.state
		after, err := rocket.Apply(before, next)
		if err != nil {
			s.log.Warn("message skipped", "channel", next.Channel, "number", next.Number,
				"type", next.Type, "reason", err)
		} else if after.Speed < 0 && before.Speed >= 0 {
			s.log.Warn("speed went negative", "channel", next.Channel, "number", next.Number,
				"speed", after.Speed)
		}
		e.state = after
		e.applied = next.Number
	}
}

// entryFor returns the channel's entry, creating it on first use.
func (s *Store) entryFor(channel string) *entry {
	s.mu.RLock()
	e, ok := s.rockets[channel]
	s.mu.RUnlock()
	if ok {
		return e
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.rockets[channel]; ok { // created while we waited for the lock
		return e
	}
	e = &entry{pending: make(map[int]rocket.Message)}
	s.rockets[channel] = e
	return e
}

// Get returns a copy of one rocket's state. ok is false if the rocket is
// unknown or has not launched yet.
func (s *Store) Get(channel string) (r rocket.Rocket, ok bool) {
	s.mu.RLock()
	e, found := s.rockets[channel]
	s.mu.RUnlock()
	if !found {
		return rocket.Rocket{}, false
	}
	r = e.snapshot()
	return r, r.Launched()
}

// List returns copies of all launched rockets, in no particular order.
func (s *Store) List() []rocket.Rocket {
	s.mu.RLock()
	entries := make([]*entry, 0, len(s.rockets))
	for _, e := range s.rockets {
		entries = append(entries, e)
	}
	s.mu.RUnlock()

	out := make([]rocket.Rocket, 0, len(entries))
	for _, e := range entries {
		if r := e.snapshot(); r.Launched() {
			out = append(out, r)
		}
	}
	return out
}

func (e *entry) snapshot() rocket.Rocket {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}
