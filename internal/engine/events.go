package engine

import (
	"net/netip"
	"time"
)

// Event is anything the engine reports. Use a type switch on the concrete
// types below.
type Event interface{ When() time.Time }

// Meta is embedded in every event.
type Meta struct{ At time.Time }

func (m Meta) When() time.Time { return m.At }

func At(t time.Time) Meta { return Meta{At: t} }

// Connected and Disconnected report the control connection to the engine.
// After Connected, callers should resync state (registrations, channels).
type Connected struct{ Meta }
type Disconnected struct {
	Meta
	Err error
}

// ChannelEntered: a channel is now under voipd control. Args are the
// arguments it entered with ("inbound", "internal", or OriginateRequest.AppArgs).
type ChannelEntered struct {
	Meta
	Channel Channel
	Args    []string
}

type ChannelStateChanged struct {
	Meta
	Channel Channel
}

// ChannelLeft: the channel left voipd control (usually because it hung up).
//
// A channel that leaves voipd's application is gone for voipd (the
// dialplan only runs the application). Asterisk does not always send
// ChannelDestroyed afterwards, e.g. for an unanswered caller that hangs up.
type ChannelLeft struct {
	Meta
	ChannelID string
}

// ChannelHangupRequested fires when a hangup was requested (e.g. a BYE
// arrived) but the channel still exists and can be queried.
type ChannelHangupRequested struct {
	Meta
	ChannelID string
	Cause     int // Q.850 cause code, 0 if unknown
}

type ChannelHangup struct {
	Meta
	ChannelID string
	Cause     int // Q.850 cause code
	CauseText string
}

type DTMF struct {
	Meta
	ChannelID string
	Digit     string
	Duration  time.Duration
}

type PlaybackFinished struct {
	Meta
	PlaybackID string
	ChannelID  string
}

// RecordingFinished: a recording ended (time limit, silence, terminating
// key or hangup); Duration has second precision.
type RecordingFinished struct {
	Meta
	Name      string
	ChannelID string
	Duration  time.Duration
}

type RecordingFailed struct {
	Meta
	Name      string
	ChannelID string
	Cause     string
}

type RegistrationChanged struct {
	Meta
	Registration Registration
}

// ContactStatus reports qualify (OPTIONS) results for an AOR contact; used for
// both trunks and registered phones.
type ContactStatus struct {
	Meta
	AOR      string
	URI      string
	Status   string // Reachable, Unreachable, Created, Removed, ...
	RTT      time.Duration
	Endpoint string
}

// RTCP carries a periodic quality report for one channel.
type RTCP struct {
	Meta
	ChannelID    string
	Direction    string // sent | received
	PacketsLost  int
	FractionLost float64 // 0..1
	Jitter       time.Duration
	RTT          time.Duration
}

// AuthFailure reports a SIP request that failed authentication: wrong
// password, unknown account, or refused by an ACL.
type AuthFailure struct {
	Meta
	IP      netip.Addr
	Account string
	Kind    string // password | unknown_account | acl
}
