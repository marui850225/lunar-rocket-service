package rocket

import (
	"errors"
	"time"
)

// Status is a rocket's lifecycle state. The zero value means not launched yet.
type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusExploded Status = "EXPLODED"
)

// Rocket is the current state of one rocket. It is a plain value: callers
// copy it freely, and Apply returns a new one.
type Rocket struct {
	ID                string
	Type              string
	Speed             int64
	Mission           string
	Status            Status
	ExplosionReason   string
	LaunchedAt        time.Time
	UpdatedAt         time.Time
	LastMessageNumber int
}

// Launched reports whether the rocket's RocketLaunched message has been applied.
// Rockets that are not launched are hidden from the API.
func (r Rocket) Launched() bool { return r.Status != "" }

// Reasons a message is skipped. The caller logs them; the message still
// counts as processed so later messages are not blocked behind it.
var (
	ErrNotLaunched     = errors.New("message before RocketLaunched")
	ErrAlreadyLaunched = errors.New("RocketLaunched for a rocket that already launched")
	ErrLaunchNotFirst  = errors.New("RocketLaunched is not message 1")
	ErrAfterExplosion  = errors.New("message after RocketExploded")
)

// Apply returns the state after applying m to r. The caller must apply
// messages of one channel in messageNumber order, each exactly once.
//
// A skipped message returns one of the Err* reasons above. Its state changes
// are not applied, but LastMessageNumber and UpdatedAt still advance.
func Apply(r Rocket, m Message) (Rocket, error) {
	r.ID = m.Channel
	r.LastMessageNumber = m.Number
	r.UpdatedAt = m.Time

	_, isLaunch := m.Payload.(Launched)
	switch {
	case !r.Launched() && !isLaunch:
		return r, ErrNotLaunched
	case !r.Launched() && m.Number != 1:
		return r, ErrLaunchNotFirst
	case r.Launched() && isLaunch:
		return r, ErrAlreadyLaunched
	case r.Status == StatusExploded:
		return r, ErrAfterExplosion
	}

	switch p := m.Payload.(type) {
	case Launched:
		r.Type = p.RocketType
		r.Speed = p.LaunchSpeed
		r.Mission = p.Mission
		r.Status = StatusActive
		r.LaunchedAt = m.Time
	case SpeedIncreased:
		r.Speed += p.By
	case SpeedDecreased:
		r.Speed -= p.By // may go negative: the messages are the source of truth
	case MissionChanged:
		r.Mission = p.NewMission
	case Exploded:
		r.Status = StatusExploded
		r.ExplosionReason = p.Reason
	}
	return r, nil
}
