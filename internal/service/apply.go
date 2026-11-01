package service

import (
	"errors"

	"rockets-service/internal/model"
)

// Reasons a message is skipped. The message still counts as processed so
// later messages are not blocked behind it.
var (
	errNotLaunched     = errors.New("message before RocketLaunched")
	errAlreadyLaunched = errors.New("RocketLaunched for a rocket that already launched")
	errLaunchNotFirst  = errors.New("RocketLaunched is not message 1")
	errAfterExplosion  = errors.New("message after RocketExploded")
)

// apply returns the state after applying m to r. Messages of one channel
// must be applied in messageNumber order, each exactly once.
//
// A skipped message returns one of the err* reasons above. Its state changes
// are not applied, but LastMessageNumber and UpdatedAt still advance.
func apply(r model.Rocket, m model.Message) (model.Rocket, error) {
	r.ID = m.Channel
	r.LastMessageNumber = m.Number
	r.UpdatedAt = m.Time

	_, isLaunch := m.Payload.(model.Launched)
	switch {
	case !r.Launched() && !isLaunch:
		return r, errNotLaunched
	case !r.Launched() && m.Number != 1:
		return r, errLaunchNotFirst
	case r.Launched() && isLaunch:
		return r, errAlreadyLaunched
	case r.Status == model.StatusExploded:
		return r, errAfterExplosion
	}

	switch p := m.Payload.(type) {
	case model.Launched:
		r.Type = p.RocketType
		r.Speed = p.LaunchSpeed
		r.Mission = p.Mission
		r.Status = model.StatusActive
		r.LaunchedAt = m.Time
	case model.SpeedIncreased:
		r.Speed += p.By
	case model.SpeedDecreased:
		r.Speed -= p.By // may go negative: the messages are the source of truth
	case model.MissionChanged:
		r.Mission = p.NewMission
	case model.Exploded:
		r.Status = model.StatusExploded
		r.ExplosionReason = p.Reason
	}
	return r, nil
}
