package rocket

import (
	"errors"
	"testing"
	"time"
)

var t0 = time.Date(2022, 2, 2, 19, 39, 5, 0, time.UTC)

func msg(n int, p Payload) Message {
	var t Type
	switch p.(type) {
	case Launched:
		t = TypeLaunched
	case SpeedIncreased:
		t = TypeSpeedIncreased
	case SpeedDecreased:
		t = TypeSpeedDecreased
	case MissionChanged:
		t = TypeMissionChanged
	case Exploded:
		t = TypeExploded
	}
	return Message{Channel: "ch-1", Number: n, Time: t0.Add(time.Duration(n) * time.Second), Type: t, Payload: p}
}

func launch(n int) Message {
	return msg(n, Launched{RocketType: "Falcon-9", LaunchSpeed: 500, Mission: "ARTEMIS"})
}

func TestApply(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message // applied in this order
		want     Rocket
		wantErr  error // error from the last message
	}{
		{
			name:     "launch sets initial state",
			messages: []Message{launch(1)},
			want: Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(1 * time.Second), LastMessageNumber: 1},
		},
		{
			name:     "speed up and down",
			messages: []Message{launch(1), msg(2, SpeedIncreased{By: 3000}), msg(3, SpeedDecreased{By: 2500})},
			want: Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 1000, Mission: "ARTEMIS", Status: StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(3 * time.Second), LastMessageNumber: 3},
		},
		{
			name:     "speed may go negative",
			messages: []Message{launch(1), msg(2, SpeedDecreased{By: 800})},
			want: Rocket{ID: "ch-1", Type: "Falcon-9", Speed: -300, Mission: "ARTEMIS", Status: StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
		},
		{
			name:     "mission changed",
			messages: []Message{launch(1), msg(2, MissionChanged{NewMission: "SHUTTLE_MIR"})},
			want: Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "SHUTTLE_MIR", Status: StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
		},
		{
			name:     "exploded keeps reason",
			messages: []Message{launch(1), msg(2, Exploded{Reason: "PRESSURE_VESSEL_FAILURE"})},
			want: Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: StatusExploded,
				ExplosionReason: "PRESSURE_VESSEL_FAILURE",
				LaunchedAt:      t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
		},
		{
			name:     "state frozen after explosion, number and time still advance",
			messages: []Message{launch(1), msg(2, Exploded{Reason: "X"}), msg(3, SpeedIncreased{By: 100})},
			want: Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: StatusExploded,
				ExplosionReason: "X",
				LaunchedAt:      t0.Add(1 * time.Second), UpdatedAt: t0.Add(3 * time.Second), LastMessageNumber: 3},
			wantErr: ErrAfterExplosion,
		},
		{
			name:     "second launch is skipped",
			messages: []Message{launch(1), msg(2, Launched{RocketType: "Saturn-V", LaunchSpeed: 1, Mission: "APOLLO"})},
			want: Rocket{ID: "ch-1", Type: "Falcon-9", Speed: 500, Mission: "ARTEMIS", Status: StatusActive,
				LaunchedAt: t0.Add(1 * time.Second), UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
			wantErr: ErrAlreadyLaunched,
		},
		{
			name:     "message 1 that is not a launch is skipped and rocket stays unlaunched",
			messages: []Message{msg(1, SpeedIncreased{By: 100})},
			want:     Rocket{ID: "ch-1", UpdatedAt: t0.Add(1 * time.Second), LastMessageNumber: 1},
			wantErr:  ErrNotLaunched,
		},
		{
			name:     "launch that is not message 1 is skipped",
			messages: []Message{msg(1, SpeedIncreased{By: 100}), launch(2)},
			want:     Rocket{ID: "ch-1", UpdatedAt: t0.Add(2 * time.Second), LastMessageNumber: 2},
			wantErr:  ErrLaunchNotFirst,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Rocket
			var err error
			for _, m := range tt.messages {
				r, err = Apply(r, m)
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
	var r Rocket
	if r.Launched() {
		t.Fatal("zero rocket reports launched")
	}
	r, _ = Apply(r, launch(1))
	if !r.Launched() {
		t.Fatal("rocket not launched after RocketLaunched")
	}
}
