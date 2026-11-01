// Package model holds the data types shared by every layer: the messages a
// rocket sends and a rocket's state. It contains no behaviour; the rules for
// decoding and applying messages live in the service package.
package model

import "time"

// MessageType is the metadata.messageType of a message.
type MessageType string

const (
	TypeLaunched       MessageType = "RocketLaunched"
	TypeSpeedIncreased MessageType = "RocketSpeedIncreased"
	TypeSpeedDecreased MessageType = "RocketSpeedDecreased"
	TypeMissionChanged MessageType = "RocketMissionChanged"
	TypeExploded       MessageType = "RocketExploded"
)

// Message is one validated message.
type Message struct {
	Channel string
	Number  int
	Time    time.Time
	Payload Payload
}

// Type returns the message type, which is determined by the payload.
func (m Message) Type() MessageType {
	if m.Payload == nil {
		return ""
	}
	return m.Payload.Type()
}

// Payload is implemented by the five message bodies. Each body knows its own
// message type, so a message's type and body can never disagree.
type Payload interface{ Type() MessageType }

// The JSON tags are the publisher's field names. They are also the format in
// which buffered messages are stored.

type Launched struct {
	RocketType  string `json:"type"`
	LaunchSpeed int64  `json:"launchSpeed"`
	Mission     string `json:"mission"`
}

type SpeedIncreased struct {
	By int64 `json:"by"`
}

type SpeedDecreased struct {
	By int64 `json:"by"`
}

type MissionChanged struct {
	NewMission string `json:"newMission"`
}

type Exploded struct {
	Reason string `json:"reason"`
}

func (Launched) Type() MessageType       { return TypeLaunched }
func (SpeedIncreased) Type() MessageType { return TypeSpeedIncreased }
func (SpeedDecreased) Type() MessageType { return TypeSpeedDecreased }
func (MissionChanged) Type() MessageType { return TypeMissionChanged }
func (Exploded) Type() MessageType       { return TypeExploded }

// Status is a rocket's lifecycle state. The zero value means not launched yet.
type Status string

const (
	StatusActive   Status = "ACTIVE"
	StatusExploded Status = "EXPLODED"
)

// Rocket is the current state of one rocket. It is a plain value, so copies
// are independent snapshots.
type Rocket struct {
	ID              string
	Type            string
	Speed           int64
	Mission         string
	Status          Status
	ExplosionReason string
	LaunchedAt      time.Time
	UpdatedAt       time.Time
	// LastMessageNumber is the publisher's messageNumber of the last message
	// processed for this rocket; every number below it has been processed too.
	LastMessageNumber int
}

// ChannelInfo is the bookkeeping of one channel: how far its messages are
// processed and which wait in the buffer. Returned as-is by GET /debug/store,
// hence the JSON tags.
type ChannelInfo struct {
	Channel           string `json:"channel"`
	Launched          bool   `json:"launched"`
	LastMessageNumber int    `json:"lastMessageNumber"`
	PendingCount      int    `json:"pendingCount"`
	// PendingNumbers lists the lowest buffered message numbers, at most 100.
	PendingNumbers []int `json:"pendingNumbers"`
}

// Launched reports whether the rocket's RocketLaunched message has been processed.
func (r Rocket) Launched() bool { return r.Status != "" }
