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

// Message is one validated message. Payload holds the body matching Type.
type Message struct {
	Channel string
	Number  int
	Time    time.Time
	Type    MessageType
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

// Launched reports whether the rocket's RocketLaunched message has been processed.
func (r Rocket) Launched() bool { return r.Status != "" }
