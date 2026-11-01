package service

import (
	"context"
	"fmt"

	"rockets-service/internal/model"
	"rockets-service/internal/storage"
)

// Result says what Ingest did with an accepted message.
type Result string

const (
	Applied   Result = "applied"   // applied now, possibly with buffered successors
	Buffered  Result = "buffered"  // stored until the gap before it fills
	Duplicate Result = "duplicate" // already processed or already buffered; ignored
)

// Ingest decodes, validates and processes one raw message. Errors wrap
// ErrInvalid for bad input; anything else is an internal failure. When
// Ingest returns without error, the outcome is committed to storage.
func (s *Service) Ingest(ctx context.Context, body []byte) (Result, error) {
	m, err := decode(body)
	if err != nil {
		return "", err
	}
	return s.ingest(ctx, m)
}

// ingest processes one validated message in a single storage transaction.
//
// Per channel, LastMessageNumber is a watermark: every message up to it has
// been processed. Messages above watermark+1 wait in the pending buffer.
// When the next message arrives it is applied together with the contiguous
// run that follows it in the buffer, e.g. with 3,4,5,7 buffered, message 2
// applies 2,3,4,5 and leaves 7 waiting for 6.
func (s *Service) ingest(ctx context.Context, m model.Message) (Result, error) {
	var res Result
	err := s.store.InTx(ctx, func(tables storage.Tables) error {
		// A channel seen for the first time has no row yet: r is then the
		// zero Rocket, with LastMessageNumber 0.
		r, _, err := tables.Rockets.Get(m.Channel)
		if err != nil {
			return err
		}
		if m.Number <= r.LastMessageNumber {
			res = Duplicate
			return nil
		}

		if m.Number > r.LastMessageNumber+1 {
			added, err := tables.Pending.Add(m)
			if err != nil {
				return err
			}
			if !added {
				res = Duplicate
				return nil
			}
			res = Buffered
			return nil
		}

		// m is the next message: apply it, then each buffered successor in
		// turn, stopping at the first missing number. Only the messages that
		// are applied are read from storage, however many wait behind a gap.
		r = s.applyAndLog(r, m)
		last := m.Number
		for {
			next, found, err := tables.Pending.Get(m.Channel, last+1)
			if err != nil {
				return err
			}
			if !found {
				break
			}
			r = s.applyAndLog(r, next)
			last = next.Number
		}
		if err := tables.Rockets.Save(r); err != nil {
			return err
		}
		if last > m.Number {
			if err := tables.Pending.Delete(m.Channel, m.Number+1, last); err != nil {
				return err
			}
		}
		res = Applied
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("channel %s message %d: %w", m.Channel, m.Number, err)
	}
	return res, nil
}

func (s *Service) applyAndLog(r model.Rocket, m model.Message) model.Rocket {
	r, err := apply(r, m)
	if err != nil {
		s.log.Warn("message skipped", "channel", m.Channel, "number", m.Number, "type", m.Type(), "reason", err)
	}
	return r
}
