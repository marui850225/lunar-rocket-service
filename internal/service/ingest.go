package service

import (
	"context"
	"fmt"

	"rockets-service/internal/model"
	"rockets-service/internal/storage"
)

// Result says what Ingest did with an accepted message.
type Result int

const (
	Applied   Result = iota // applied now, possibly with buffered successors
	Buffered                // stored until the gap before it fills
	Duplicate               // already processed or already buffered; ignored
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

// Ingest decodes, validates and processes one raw message. Errors wrap
// ErrInvalid (bad input) or ErrBufferFull (redeliver later); anything else
// is an internal failure. When Ingest returns without error, the outcome is
// committed to storage.
func (s *Service) Ingest(ctx context.Context, body []byte) (Result, error) {
	m, err := decode(body)
	if err != nil {
		return 0, err
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
	err := s.store.InChannel(ctx, m.Channel, func(tx storage.Tx) error {
		r, err := tx.Rocket()
		if err != nil {
			return err
		}
		if m.Number <= r.LastMessageNumber {
			res = Duplicate
			return nil
		}

		if m.Number > r.LastMessageNumber+1 {
			added, err := tx.AddPending(m)
			if err != nil {
				return err
			}
			if !added {
				res = Duplicate
				return nil
			}
			// Checked after adding so a duplicate is never refused as "full".
			// Returning the error rolls the insert back.
			n, err := tx.CountPending()
			if err != nil {
				return err
			}
			if n > s.maxPending {
				return ErrBufferFull
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
			next, found, err := tx.Pending(last + 1)
			if err != nil {
				return err
			}
			if !found {
				break
			}
			r = s.applyAndLog(r, next)
			last = next.Number
		}
		if err := tx.SaveRocket(r); err != nil {
			return err
		}
		if last > m.Number {
			if err := tx.DeletePending(m.Number+1, last); err != nil {
				return err
			}
		}
		res = Applied
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("channel %s message %d: %w", m.Channel, m.Number, err)
	}
	s.log.Debug("message ingested", "channel", m.Channel, "number", m.Number, "type", m.Type(), "result", res)
	return res, nil
}

func (s *Service) applyAndLog(before model.Rocket, m model.Message) model.Rocket {
	after, err := apply(before, m)
	if err != nil {
		s.log.Warn("message skipped", "channel", m.Channel, "number", m.Number, "type", m.Type(), "reason", err)
	} else if after.Speed < 0 && before.Speed >= 0 {
		s.log.Warn("speed went negative", "channel", m.Channel, "number", m.Number, "speed", after.Speed)
	}
	return after
}
