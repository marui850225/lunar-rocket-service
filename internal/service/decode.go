package service

import (
	"encoding/json"
	"fmt"
	"time"

	"rockets-service/internal/model"
)

// Wire format. Pointers distinguish a missing field from a zero value.
type envelope struct {
	Metadata struct {
		Channel       string            `json:"channel"`
		MessageNumber int               `json:"messageNumber"`
		MessageTime   time.Time         `json:"messageTime"`
		MessageType   model.MessageType `json:"messageType"`
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

// decode parses and validates one message. Any failure wraps ErrInvalid.
// Unknown JSON fields are ignored so senders can add fields without breaking us.
func decode(data []byte) (model.Message, error) {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return model.Message{}, invalid("malformed JSON: %v", err)
	}
	md := env.Metadata
	switch {
	case md.Channel == "":
		return model.Message{}, invalid("metadata.channel is required")
	case md.MessageNumber < 1:
		return model.Message{}, invalid("metadata.messageNumber must be >= 1, got %d", md.MessageNumber)
	case md.MessageTime.IsZero():
		return model.Message{}, invalid("metadata.messageTime is required")
	case len(env.Message) == 0:
		return model.Message{}, invalid("message body is required")
	}

	payload, err := decodePayload(md.MessageType, env.Message)
	if err != nil {
		return model.Message{}, err
	}
	return model.Message{
		Channel: md.Channel,
		Number:  md.MessageNumber,
		Time:    md.MessageTime,
		Type:    md.MessageType,
		Payload: payload,
	}, nil
}

func decodePayload(t model.MessageType, raw json.RawMessage) (model.Payload, error) {
	switch t {
	case model.TypeLaunched:
		var w wireLaunched
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.Type == nil || *w.Type == "" || w.Mission == nil || *w.Mission == "" || w.LaunchSpeed == nil {
			return nil, invalid("%s requires type, launchSpeed and mission", t)
		}
		return model.Launched{RocketType: *w.Type, LaunchSpeed: *w.LaunchSpeed, Mission: *w.Mission}, nil

	case model.TypeSpeedIncreased, model.TypeSpeedDecreased:
		var w wireSpeed
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.By == nil || *w.By < 0 {
			return nil, invalid("%s requires a non-negative by", t)
		}
		if t == model.TypeSpeedIncreased {
			return model.SpeedIncreased{By: *w.By}, nil
		}
		return model.SpeedDecreased{By: *w.By}, nil

	case model.TypeMissionChanged:
		var w wireMission
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.NewMission == nil || *w.NewMission == "" {
			return nil, invalid("%s requires newMission", t)
		}
		return model.MissionChanged{NewMission: *w.NewMission}, nil

	case model.TypeExploded:
		var w wireExploded
		if err := unmarshalBody(raw, &w); err != nil {
			return nil, err
		}
		if w.Reason == nil {
			return nil, invalid("%s requires reason", t)
		}
		return model.Exploded{Reason: *w.Reason}, nil

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
