package service

import (
	"errors"
	"rockets-service/internal/model"
	"testing"
	"time"
)

func msg(n int, p model.Payload) model.Message {
	var t model.MessageType
	switch p.(type) {
	case model.Launched:
		t = model.TypeLaunched
	case model.SpeedIncreased:
		t = model.TypeSpeedIncreased
	case model.SpeedDecreased:
		t = model.TypeSpeedDecreased
	case model.MissionChanged:
		t = model.TypeMissionChanged
	case model.Exploded:
		t = model.TypeExploded
	}
	return model.Message{Channel: "ch-1", Number: n, Time: t0.Add(time.Duration(n) * time.Second), Type: t, Payload: p}
}

func launchN(n int) model.Message {
	return msg(n, model.Launched{RocketType: "Falcon-9", LaunchSpeed: 500, Mission: "ARTEMIS"})
}

func TestApply(t *testing.T) {
	tests := []struct {
		name     string
		messages []model.Message // applied in this order
		want     model.Rocket
		wantErr  error // error from the last message
	}{
		{
			name:     "launch sets initial state",
			messages: []model.Message{launchN(1)},
			want: model.Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: model.StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(1 * time.Second), LastMessageNumber: 1},
		},
		{
			name:     "speed up and down",
			messages: []model.Message{launchN(1), msg(2, model.SpeedIncreased{By: 3000}), msg(3, model.SpeedDecreased{By: 2500})},
			want: model.Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 1000, Mission: "ARTEMIS", Status: model.StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(3 * time.Second), LastMessageNumber: 3},
		},
		{
			name:     "speed may go negative",
			messages: []model.Message{launchN(1), msg(2, model.SpeedDecreased{By: 800})},
			want: model.Rocket{ID: "ch-1", Type: "Falcon-9", Speed: -300, Mission: "ARTEMIS", Status: model.StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
		},
		{
			name:     "mission changed",
			messages: []model.Message{launchN(1), msg(2, model.MissionChanged{NewMission: "SHUTTLE_MIR"})},
			want: model.Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "SHUTTLE_MIR", Status: model.StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
		},
		{
			name:     "exploded keeps reason",
			messages: []model.Message{launchN(1), msg(2, model.Exploded{Reason: "PRESSURE_VESSEL_FAILURE"})},
			want: model.Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: model.StatusExploded,
				ExplosionReason: "PRESSURE_VESSEL_FAILURE",
				LaunchedAt:      t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
		},
		{
			name:     "state frozen after explosion, number and time still advance",
			messages: []model.Message{launchN(1), msg(2, model.Exploded{Reason: "X"}), msg(3, model.SpeedIncreased{By: 100})},
			want: model.Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: model.StatusExploded,
				ExplosionReason: "X",
				LaunchedAt:      t0.Add(1 * time.Second), UpdatedAt: t0.Add(3 * time.Second), LastMessageNumber: 3},
			wantErr: errAfterExplosion,
		},
		{
			name:     "second launch is skipped",
			messages: []model.Message{launchN(1), msg(2, model.Launched{RocketType: "Saturn-V", LaunchSpeed: 1, Mission: "APOLLO"})},
			want: model.Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: model.StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
			wantErr: errAlreadyLaunched,
		},
		{
			name:     "message 1 that is not a launch is skipped and rocket stays unlaunched",
			messages: []model.Message{msg(1, model.SpeedIncreased{By: 100})},
			want:     model.Rocket{ID: "ch-1", UpdatedAt: t0.Add(1 * time.Second), LastMessageNumber: 1},
			wantErr:  errNotLaunched,
		},
		{
			name:     "launch that is not message 1 is skipped",
			messages: []model.Message{msg(1, model.SpeedIncreased{By: 100}), launchN(2)},
			want:     model.Rocket{ID: "ch-1", UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
			wantErr:  errLaunchNotFirst,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r model.Rocket
			var err error
			for _, m := range tt.messages {
				r, err = apply(r, m)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("last error = %v, want %v", err, tt.wantErr)
			}
			if r != tt.want {
				t.Fatalf("state =\n  %+v\nwant\n  %+v", r, tt.want)
			}
		})
	}
}

func TestLaunched(t *testing.T) {
	var r model.Rocket
	if r.Launched() {
		t.Fatal("zero rocket reports launched")
	}
	r, _ = apply(r, launchN(1))
	if !r.Launched() {
		t.Fatal("rocket not launched after RocketLaunched")
	}
}
