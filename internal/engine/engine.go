// Package engine defines the boundary between voipd's call logic and the
// SIP/media engine. Nothing outside engine/<impl> may depend on Asterisk
// types, so the engine can be replaced without touching call flows.
package engine

import (
	"context"
	"time"
)

type Engine interface {
	// Run connects to the engine and keeps the connection alive, emitting
	// events until ctx is cancelled. It reconnects with backoff on its own.
	Run(ctx context.Context) error
	Events() <-chan Event

	// Call control.
	Ring(ctx context.Context, channelID string) error
	Answer(ctx context.Context, channelID string) error
	Hangup(ctx context.Context, channelID string, reason HangupReason) error
	// Originate creates a channel under voipd control and dials it. The
	// channel enters with ChannelEntered (Args = req.AppArgs) right away,
	// then reports Ringing/Up via ChannelStateChanged or ChannelHangup.
	Originate(ctx context.Context, req OriginateRequest) error
	CreateBridge(ctx context.Context) (bridgeID string, err error)
	AddToBridge(ctx context.Context, bridgeID string, channelIDs ...string) error
	DestroyBridge(ctx context.Context, bridgeID string) error
	Play(ctx context.Context, channelID, media string) (playbackID string, err error)
	ChannelVar(ctx context.Context, channelID, name string) (string, error)
	// ChannelExists is used to resync after a reconnect.
	ChannelExists(ctx context.Context, channelID string) (bool, error)

	// Configuration and registrations.
	ApplyConfig(ctx context.Context, cfg Config) error
	Registrations(ctx context.Context) ([]Registration, error)
	Reregister(ctx context.Context, registration string) error
	// ReloadRegistrations reloads the outbound registration module, which
	// recreates every registration from scratch (new transport/DNS state).
	ReloadRegistrations(ctx context.Context) error
	// RestartWhenIdle restarts the engine as soon as no calls are active.
	// Last resort of the trunk supervisor.
	RestartWhenIdle(ctx context.Context) error
}

type HangupReason string

const (
	HangupNormal      HangupReason = "normal"
	HangupBusy        HangupReason = "busy"
	HangupNoAnswer    HangupReason = "no_answer"
	HangupRejected    HangupReason = "rejected"
	HangupCongestion  HangupReason = "congestion"
	HangupUnallocated HangupReason = "unallocated"
	// HangupAnsweredElsewhere tells phones not to log a missed call.
	HangupAnsweredElsewhere HangupReason = "answered_elsewhere"
)

// Q.850 cause codes voipd interprets.
const (
	CauseUnallocated      = 1
	CauseNormal           = 16
	CauseBusy             = 17
	CauseNoUserResponse   = 18
	CauseNoAnswer         = 19
	CauseSubscriberAbsent = 20
	CauseRejected         = 21
	CauseCongestion       = 34
)

type Channel struct {
	ID       string
	Name     string // e.g. PJSIP/kitchen-00000001
	State    string // Down, Ring, Ringing, Up, ...
	Caller   Party
	Dialed   string // extension the channel entered Stasis with
	Endpoint string // PJSIP endpoint id: SIP username for phones, "trunk-<name>-<line>" for trunks
}

type Party struct {
	Name   string
	Number string
}

type OriginateRequest struct {
	ID       string        // channel id to use (caller-chosen, unique)
	Endpoint string        // PJSIP endpoint, e.g. "kitchen"
	Dial     string        // optional user part: PJSIP/<dial>@<endpoint>
	Caller   Party         // caller ID presented to the callee
	Timeout  time.Duration // ring timeout
	AppArgs  []string      // passed back in ChannelEntered.Args
	// Originator is the channel this call is placed on behalf of; the new
	// channel prefers its codec, avoiding transcoding.
	Originator string
	// Vars are channel variables set on the new channel (e.g. PJSIP headers).
	Vars map[string]string
}

// Registration is the state of one outbound registration to a trunk.
type Registration struct {
	Name      string // PJSIP registration id
	ServerURI string
	State     RegState
	// Detail carries the raw engine status for diagnostics.
	Detail string
}

type RegState string

const (
	RegRegistered   RegState = "registered"
	RegUnregistered RegState = "unregistered"
	RegRejected     RegState = "rejected"
	RegFailed       RegState = "failed" // engine stopped retrying
	// RegUnreachable: registered, but the registrar stopped answering
	// qualify OPTIONS (only for trunks that qualify).
	RegUnreachable RegState = "unreachable"
	RegUnknown     RegState = "unknown"
)
