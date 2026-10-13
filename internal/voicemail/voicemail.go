// Package voicemail manages mailboxes: message files in the recording
// directory shared with Asterisk, message waiting lights (MWI), personal
// greetings. The call flow records messages and runs the phone menu; the
// web API lists, plays and deletes them; both go through this package so
// files, database and phones stay consistent.
package voicemail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"voipd/internal/engine"
	"voipd/internal/events"
	"voipd/internal/store"
)

// MWI publishes message waiting state to phones.
type MWI interface {
	SetMWI(ctx context.Context, mailbox string, newMessages, oldMessages int) error
}

type Service struct {
	st  *store.Store
	mwi MWI
	bus *events.Bus
	dir string
	log *slog.Logger
	Now func() time.Time
}

func New(st *store.Store, mwi MWI, bus *events.Bus, dir string, log *slog.Logger) *Service {
	return &Service{st: st, mwi: mwi, bus: bus, dir: dir, log: log, Now: time.Now}
}

// MessageName is the recording name of a message ("vm/<ext>/<id>").
func MessageName(extension, id string) string { return "vm/" + extension + "/" + id }

// GreetingName is the recording name of an extension's personal greeting.
func GreetingName(extension string) string { return "greet/" + extension }

// NewGreetingName is where a greeting recorded on the phone waits to be
// confirmed.
func NewGreetingName(extension string) string { return "greet/" + extension + "-new" }

// MessagePath is the WAV file of a message.
func (s *Service) MessagePath(v store.Voicemail) string {
	return filepath.Join(s.dir, MessageName(v.Extension, v.ID)+".wav")
}

// Prepare creates a mailbox's directories before Asterisk records into
// them. They belong to voipd and are group-writable for Asterisk (setgid
// recording directory, see the Asterisk entrypoint).
func (s *Service) Prepare(extension string) error {
	for _, d := range []string{filepath.Join(s.dir, "vm", extension), filepath.Join(s.dir, "greet")} {
		if err := os.MkdirAll(d, 0o2775); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o2775); err != nil {
			return err
		}
	}
	return nil
}

// UpdateMWI publishes an extension's counts to its phones and the bus.
func (s *Service) UpdateMWI(ctx context.Context, extensionID int64) error {
	c, err := s.st.MailboxCountOf(ctx, extensionID)
	if err != nil {
		return err
	}
	return s.publish(ctx, c)
}

// PublishAll publishes every mailbox's counts (after the engine connects).
func (s *Service) PublishAll(ctx context.Context) error {
	cs, err := s.st.MailboxCounts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range cs {
		errs = append(errs, s.publish(ctx, c))
	}
	return errors.Join(errs...)
}

func (s *Service) publish(ctx context.Context, c store.MailboxCount) error {
	if s.bus != nil {
		s.bus.Publish(events.Event{Type: events.VoicemailCount, At: s.Now(),
			Data: map[string]any{"extension": c.Extension, "new": c.New, "old": c.Old}})
	}
	if s.mwi == nil {
		return nil
	}
	return s.mwi.SetMWI(ctx, engine.Mailbox(c.Extension), c.New, c.Old)
}

// SetHeard marks a message heard (or new) and updates the lights.
func (s *Service) SetHeard(ctx context.Context, v store.Voicemail, heard bool) error {
	if v.Heard == heard {
		return nil
	}
	if err := s.st.SetHeard(ctx, v.ID, heard, s.Now()); err != nil {
		return err
	}
	return s.UpdateMWI(ctx, v.ExtensionID)
}

// Delete removes a message, its file and updates the lights.
func (s *Service) Delete(ctx context.Context, v store.Voicemail) error {
	if err := s.st.DeleteVoicemail(ctx, v.ID); err != nil {
		return err
	}
	if err := os.Remove(s.MessagePath(v)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Warn("delete voicemail file", "id", v.ID, "err", err)
	}
	return s.UpdateMWI(ctx, v.ExtensionID)
}

// RemoveMailbox deletes the files of an extension that was deleted (its
// messages are deleted with it by the database).
func (s *Service) RemoveMailbox(extension string) {
	if extension == "" {
		return
	}
	os.RemoveAll(filepath.Join(s.dir, "vm", extension))
	for _, ext := range []string{".wav", ".wav16"} {
		os.Remove(filepath.Join(s.dir, GreetingName(extension)+ext))
	}
}

// --- greetings ---

// GreetingMedia is what to play to callers: the personal greeting if
// there is one, else the default prompt.
func (s *Service) GreetingMedia(e store.Extension) string {
	if e.VoicemailGreeting {
		return "recording:" + GreetingName(e.Number)
	}
	return "sound:voipd/vm-greeting"
}

// GreetingPath returns the greeting file of an extension, if any.
func (s *Service) GreetingPath(e store.Extension) (string, bool) {
	for _, ext := range []string{".wav16", ".wav"} {
		p := filepath.Join(s.dir, GreetingName(e.Number)+ext)
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// KeepNewGreeting makes the greeting just recorded on the phone the
// personal greeting.
func (s *Service) KeepNewGreeting(ctx context.Context, e store.Extension) error {
	src := filepath.Join(s.dir, NewGreetingName(e.Number)+".wav")
	s.removeGreetingFiles(e.Number)
	if err := os.Rename(src, filepath.Join(s.dir, GreetingName(e.Number)+".wav")); err != nil {
		return err
	}
	return s.st.SetGreeting(ctx, e.ID, true)
}

// NewGreetingPath is the file of a greeting recorded on the phone that
// waits for confirmation.
func (s *Service) NewGreetingPath(e store.Extension) string {
	return filepath.Join(s.dir, NewGreetingName(e.Number)+".wav")
}

// DiscardNewGreeting removes a greeting recorded on the phone.
func (s *Service) DiscardNewGreeting(e store.Extension) {
	os.Remove(filepath.Join(s.dir, NewGreetingName(e.Number)+".wav"))
}

// SaveGreeting stores an uploaded greeting: a PCM WAV, mono, 16 bit,
// 8 or 16 kHz, at most MaxGreeting long.
func (s *Service) SaveGreeting(ctx context.Context, e store.Extension, r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, maxGreetingBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxGreetingBytes {
		return ErrBadGreeting{"the file is too large (at most 2 minutes)"}
	}
	info, err := ParseWAV(data)
	if err != nil {
		return ErrBadGreeting{err.Error()}
	}
	if info.Duration > MaxGreeting {
		return ErrBadGreeting{fmt.Sprintf("the greeting is %s long; at most %s", info.Duration.Round(time.Second), MaxGreeting)}
	}
	if err := s.Prepare(e.Number); err != nil {
		return err
	}
	ext := ".wav"
	if info.SampleRate == 16000 {
		ext = ".wav16" // Asterisk picks the format by extension
	}
	tmp := filepath.Join(s.dir, GreetingName(e.Number)+".upload")
	if err := os.WriteFile(tmp, data, 0o664); err != nil {
		return err
	}
	s.removeGreetingFiles(e.Number)
	if err := os.Rename(tmp, filepath.Join(s.dir, GreetingName(e.Number)+ext)); err != nil {
		return err
	}
	return s.st.SetGreeting(ctx, e.ID, true)
}

// DeleteGreeting goes back to the default greeting.
func (s *Service) DeleteGreeting(ctx context.Context, e store.Extension) error {
	s.removeGreetingFiles(e.Number)
	return s.st.SetGreeting(ctx, e.ID, false)
}

func (s *Service) removeGreetingFiles(extension string) {
	for _, ext := range []string{".wav", ".wav16"} {
		os.Remove(filepath.Join(s.dir, GreetingName(extension)+ext))
	}
}

// ErrBadGreeting: an uploaded greeting cannot be used; the message says why.
type ErrBadGreeting struct{ Msg string }

func (e ErrBadGreeting) Error() string { return e.Msg }
