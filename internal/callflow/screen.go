package callflow

import (
	"context"

	"voipd/internal/numbers"
	"voipd/internal/store"
)

// Verdict is a screening decision for an inbound call.
type Verdict struct {
	Block  bool
	Reason string // shown in history, e.g. "blocklist:phoneblock" or "anonymous"
	// EntryID is the list entry that matched (blocked or allowed), if any.
	EntryID int64
	// CallerName is the caller's name from contacts or the list, if known.
	CallerName string
}

// Screener decides whether an inbound call is blocked. Blocked calls are
// handled in stealth mode: the caller hears ringing, no extension rings.
type Screener interface {
	Screen(ctx context.Context, caller numbers.Number, line store.Line) Verdict
}

type AllowAll struct{}

func (AllowAll) Screen(context.Context, numbers.Number, store.Line) Verdict { return Verdict{} }

// ScreenerFunc adapts a function to Screener.
type ScreenerFunc func(ctx context.Context, caller numbers.Number, line store.Line) Verdict

func (f ScreenerFunc) Screen(ctx context.Context, caller numbers.Number, line store.Line) Verdict {
	return f(ctx, caller, line)
}
