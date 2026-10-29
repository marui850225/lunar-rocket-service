// Package rocket holds the domain model: the messages a rocket sends and the
// pure rules for applying them to a rocket's state. It knows nothing about
// HTTP, ordering, duplicates or locking.
package rocket

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Type is the messageType of a message.
type Type string

const (
	TypeLaunched       Type = "RocketLaunched"
	TypeSpeedIncreased Type = "RocketSpeedIncreased"
	TypeSpeedDecreased Type = "RocketSpeedDecreased"
	TypeMissionChanged Type = "RocketMissionChanged"
	TypeExploded       Type = "RocketExploded"
)

// Message is one validated message. Payload holds one of the payload types
// below, matching Type.
type Message struct {
	Channel string
	Number  int
	Time    time.Time
	Type    Type
	Payload Payload
}

// Payload is implemented by the five message bodies.
type Payload interface{ isPayload() }

type Launched struct {
	RocketType  string
	LaunchSpeed int64
	Mission     string
}

type SpeedIncreased struct{ By int64 }

type SpeedDecreased struct{ By int64 }

type MissionChanged struct{ NewMission string }

type Exploded struct{ Reason string }

func (Launched) isPayload()       {}
func (SpeedIncreased) isPayload() {}
func (SpeedDecreased) isPayload() {}
func (MissionChanged) isPayload() {}
func (Exploded) isPayload()       {}

// ErrInvalid wraps every validation failure returned by Decode.
var ErrInvalid = errors.New("invalid message")

// Wire format. Pointers distinguish a missing field from a zero value.
type envelope struct {
	Metadata struct {
		Channel       string    `json:"channel"`
		MessageNumber int       `json:"messageNumber"`
		MessageTime   time.Time `json:"messageTime"`
		MessageType   Type      `json:"messageType"`
	} `json:"metadata"`
	Message json.RawMessage `json:"message"`
}

type wireLaunched struct {
	Type        *string `json:"type"`
	LaunchSpeed *int64  `json:"launchSpeed"`
	Mission     *string `json:"mission"`
}

type wireSpeed struct {
	By *int64 `json:"by"`
}

type wireMission struct {
	NewMission *string `json:"newMission"`
}

type wireExploded struct {
	Reason *string `json:"reason"`
}

// Decode parses and validates one message. Any failure wraps ErrInvalid.
// Unknown JSON fields are ignored so senders can add fields without breaking us.
func Decode(data []byte) (Message, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Message{}, invalid("malformed JSON: %v", err)
	}
	md := env.Metadata
	switch {
	case md.Channel == "":
		return Message{}, invalid("metadata.channel is required")
	case md.MessageNumber < 1:
		return Message{}, invalid("metadata.messageNumber must be >= 1, got %d", md.MessageNumber)
	case md.MessageTime.IsZero():
		return Message{}, invalid("metadata.messageTime is required")
	case len(env.Message) == 0:
		return Message{}, invalid("message body is required")
	}

	payload, err := decodePayload(md.MessageType, env.Message)
	if err != nil {
		return Message{}, err
	}
	return Message{
		Channel: md.Channel,
		Number:  md.MessageNumber,
		Time:    md.MessageTime,
		Type:    md.MessageType,
		Payload: payload,
	}, nil
}

func decodePayload(t Type, raw json.RawMessage) (Payload, error) {
	switch t {
	case TypeLaunched:
		var w wireLaunched
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.Type == nil || *w.Type == "" || w.Mission == nil || *w.Mission == "" || w.LaunchSpeed == nil {
			return nil, invalid("%s requires type, launchSpeed and mission", t)
		}
		return Launched{RocketType: *w.Type, LaunchSpeed: *w.LaunchSpeed, Mission: *w.Mission}, nil

	case TypeSpeedIncreased, TypeSpeedDecreased:
		var w wireSpeed
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.By == nil || *w.By < 0 {
			return nil, invalid("%s requires a non-negative by", t)
		}
		if t == TypeSpeedIncreased {
			return SpeedIncreased{By: *w.By}, nil
		}
		return SpeedDecreased{By: *w.By}, nil

	case TypeMissionChanged:
		var w wireMission
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.NewMission == nil || *w.NewMission == "" {
			return nil, invalid("%s requires newMission", t)
		}
		return MissionChanged{NewMission: *w.NewMission}, nil

	case TypeExploded:
		var w wireExploded
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.Reason == nil {
			return nil, invalid("%s requires reason", t)
		}
		return Exploded{Reason: *w.Reason}, nil

	case "":
		return nil, invalid("metadata.messageType is required")
	default:
		return nil, invalid("unknown messageType %q", t)
	}
}

func unmarshalBody(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return invalid("malformed message body: %v", err)
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
