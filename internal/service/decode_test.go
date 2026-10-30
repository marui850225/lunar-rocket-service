package service

import (
	"errors"
	"rockets-service/internal/model"
	"strings"
	"testing"
	"time"
)

func body(msgType, payload string) string {
	return `{"metadata":{"channel":"ch-1","messageNumber":7,"messageTime":"2022-02-02T19:39:05.86337+01:00","messageType":"` +
		msgType + `"},"message":` + payload + `}`
}

func TestDecodeValid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want model.Payload
	}{
		{"launched", body("RocketLaunched", `{"type":"Falcon-9","launchSpeed":500,"mission":"ARTEMIS"}`),
			model.Launched{RocketType: "Falcon-9", LaunchSpeed: 500, Mission: "ARTEMIS"}},
		{"launched with zero speed", body("RocketLaunched", `{"type":"Falcon-9","launchSpeed":0,"mission":"ARTEMIS"}`),
			model.Launched{RocketType: "Falcon-9", LaunchSpeed: 0, Mission: "ARTEMIS"}},
		{"speed increased", body("RocketSpeedIncreased", `{"by":3000}`), model.SpeedIncreased{By: 3000}},
		{"speed decreased", body("RocketSpeedDecreased", `{"by":2500}`), model.SpeedDecreased{By: 2500}},
		{"mission changed", body("RocketMissionChanged", `{"newMission":"SHUTTLE_MIR"}`), model.MissionChanged{NewMission: "SHUTTLE_MIR"}},
		{"exploded", body("RocketExploded", `{"reason":"PRESSURE_VESSEL_FAILURE"}`), model.Exploded{Reason: "PRESSURE_VESSEL_FAILURE"}},
		{"unknown fields ignored", body("RocketSpeedIncreased", `{"by":1,"extra":true}`), model.SpeedIncreased{By: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := decode([]byte(tt.in))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if m.Payload != tt.want {
				t.Fatalf("payload = %+v, want %+v", m.Payload, tt.want)
			}
			wantTime, _ := time.Parse(time.RFC3339Nano, "2022-02-02T19:39:05.86337+01:00")
			if m.Channel != "ch-1" || m.Number != 7 || !m.Time.Equal(wantTime) {
				t.Fatalf("metadata = %q %d %v", m.Channel, m.Number, m.Time)
			}
		})
	}
}

func TestDecodeInvalid(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantMsg string
	}{
		{"not JSON", `{nope`, "malformed JSON"},
		{"missing channel", `{"metadata":{"messageNumber":1,"messageTime":"2022-02-02T19:39:05Z","messageType":"RocketExploded"},"message":{"reason":"x"}}`, "channel"},
		{"message number zero", strings.Replace(body("RocketExploded", `{"reason":"x"}`), `"messageNumber":7`, `"messageNumber":0`, 1), "messageNumber"},
		{"missing time", strings.Replace(body("RocketExploded", `{"reason":"x"}`), `"messageTime":"2022-02-02T19:39:05.86337+01:00",`, ``, 1), "messageTime"},
		{"missing type", body("", `{"reason":"x"}`), "messageType is required"},
		{"unknown type", body("RocketLanded", `{}`), "unknown messageType"},
		{"missing body", `{"metadata":{"channel":"c","messageNumber":1,"messageTime":"2022-02-02T19:39:05Z","messageType":"RocketExploded"}}`, "body is required"},
		{"body wrong shape", body("RocketSpeedIncreased", `{"by":"fast"}`), "malformed message body"},
		{"launched missing speed", body("RocketLaunched", `{"type":"Falcon-9","mission":"ARTEMIS"}`), "launchSpeed"},
		{"launched empty mission", body("RocketLaunched", `{"type":"Falcon-9","launchSpeed":1,"mission":""}`), "mission"},
		{"speed missing by", body("RocketSpeedIncreased", `{}`), "non-negative by"},
		{"speed negative by", body("RocketSpeedDecreased", `{"by":-5}`), "non-negative by"},
		{"mission missing", body("RocketMissionChanged", `{}`), "newMission"},
		{"exploded missing reason", body("RocketExploded", `{}`), "reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decode([]byte(tt.in))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("err = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestDecodeRealSample(t *testing.T) {
	// Captured verbatim from the rockets binary (seed 444).
	in := `{"metadata":{"channel":"af465720-41b1-559a-adeb-4b86eb5df254","messageNumber":1,"messageType":"RocketLaunched","messageTime":"2026-10-29T09:20:06.677197834+01:00"},"message":{"type":"Athena","launchSpeed":500,"mission":"APOLLO"}}`
	m, err := decode([]byte(in))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if m.Type() != model.TypeLaunched || m.Payload != (model.Launched{RocketType: "Athena", LaunchSpeed: 500, Mission: "APOLLO"}) {
		t.Fatalf("got %+v", m)
	}
}
