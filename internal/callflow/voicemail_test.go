package callflow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voipd/internal/engine"
	"voipd/internal/store"
)

func (f *fakeEngine) played(id, pb string) {
	f.send(engine.PlaybackFinished{Meta: at(), PlaybackID: pb, ChannelID: id})
}

func (f *fakeEngine) dtmf(id, digit string) {
	f.send(engine.DTMF{Meta: at(), ChannelID: id, Digit: digit})
}

func (f *fakeEngine) recorded(id, name string, d time.Duration) {
	f.send(engine.RecordingFinished{Meta: at(), Name: name, ChannelID: id, Duration: d})
}

// recordName expects "record <id> <name>" and returns the name.
func (f *fakeEngine) recordName(t *testing.T, id string) string {
	t.Helper()
	got := f.next(t)
	name, ok := strings.CutPrefix(got, "record "+id+" ")
	if !ok {
		t.Fatalf("action = %q, want a recording of %s", got, id)
	}
	return name
}

// ringOut lets the inbound call to the home group time out (1s).
func ringOut(t *testing.T, e *env) {
	t.Helper()
	e.eng.expect(t, "ring A")
	e.eng.expect(t, "originate kitchen")
	e.eng.expect(t, "originate office")
	got := map[string]bool{e.eng.next(t): true, e.eng.next(t): true}
	for _, ep := range []string{"kitchen", "office"} {
		id := e.eng.originated(t, ep).ID
		if !got["hangup "+id+" no_answer"] {
			t.Fatalf("leg %s not released: %v", ep, got)
		}
		e.eng.hangup(id, 16)
	}
}

func voicemailEnv(t *testing.T) *env {
	e := newEnv(t, 1, "voicemail")
	if _, err := e.st.DB.Exec(`UPDATE ring_groups SET fallback_extension_id = 1`); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestVoicemailTakesMessage(t *testing.T) {
	e := voicemailEnv(t)
	e.inbound("A", "01701234567")
	ringOut(t, e)
	e.eng.expect(t, "answer A")
	e.eng.expect(t, "play A sound:voipd/vm-greeting,sound:voipd/beep")
	e.eng.played("A", "pb1")
	name := e.eng.recordName(t, "A")
	if !strings.HasPrefix(name, "vm/10/") {
		t.Fatalf("recording name %q", name)
	}
	if fi, err := os.Stat(filepath.Join(e.vmDir, "vm", "10")); err != nil || !fi.IsDir() {
		t.Errorf("mailbox dir not prepared: %v", err)
	}
	// The caller presses # to finish: thanks, then hang up.
	e.eng.recorded("A", name, 4*time.Second)
	e.eng.expect(t, "mwi 10@voipd 1/0")
	e.eng.expect(t, "play A sound:voipd/vm-thanks")
	e.eng.played("A", "pb2")
	e.eng.expect(t, "hangup A normal")
	e.eng.hangup("A", engine.CauseNormal)

	evs := e.events(t)
	if got := strings.Join(types(evs), ","); strings.Contains(got, "call.missed") {
		t.Errorf("a message is not a missed call: %s", got)
	}
	c := e.onlyCall(t)
	if c.Status != store.CallVoicemail {
		t.Errorf("status = %s", c.Status)
	}
	vms, _ := e.st.Voicemails(context.Background(), nil)
	if len(vms) != 1 || vms[0].Extension != "10" || vms[0].Caller != "+491701234567" || vms[0].DurationMS != 4000 ||
		vms[0].CallID != c.ID || "vm/10/"+vms[0].ID != name {
		t.Errorf("voicemails = %+v", vms)
	}
}

func TestVoicemailCallerHangsUp(t *testing.T) {
	t.Run("during the greeting: missed", func(t *testing.T) {
		e := voicemailEnv(t)
		e.inbound("A", "01701234567")
		ringOut(t, e)
		e.eng.expect(t, "answer A")
		e.eng.expect(t, "play A sound:voipd/vm-greeting,sound:voipd/beep")
		e.eng.hangup("A", engine.CauseNormal)
		e.events(t)
		if c := e.onlyCall(t); c.Status != store.CallMissed {
			t.Errorf("status = %s", c.Status)
		}
	})
	t.Run("after speaking: message kept", func(t *testing.T) {
		e := voicemailEnv(t)
		e.inbound("A", "01701234567")
		ringOut(t, e)
		e.eng.expect(t, "answer A")
		e.eng.expect(t, "play A sound:voipd/vm-greeting,sound:voipd/beep")
		e.eng.played("A", "pb1")
		name := e.eng.recordName(t, "A")
		e.eng.hangup("A", engine.CauseNormal)
		e.eng.recorded("A", name, 7*time.Second) // arrives after the hangup
		e.eng.expect(t, "mwi 10@voipd 1/0")
		e.events(t)
		if c := e.onlyCall(t); c.Status != store.CallVoicemail {
			t.Errorf("status = %s", c.Status)
		}
	})
	t.Run("silence only: discarded", func(t *testing.T) {
		e := voicemailEnv(t)
		e.inbound("A", "01701234567")
		ringOut(t, e)
		e.eng.expect(t, "answer A")
		e.eng.expect(t, "play A sound:voipd/vm-greeting,sound:voipd/beep")
		e.eng.played("A", "pb1")
		name := e.eng.recordName(t, "A")
		e.eng.hangup("A", engine.CauseNormal)
		e.eng.recorded("A", name, 0)
		evs := e.events(t)
		if !strings.Contains(strings.Join(types(evs), ","), "call.missed") {
			t.Errorf("events = %v", types(evs))
		}
		if vms, _ := e.st.Voicemails(context.Background(), nil); len(vms) != 0 {
			t.Errorf("empty message kept: %+v", vms)
		}
	})
}

func addMessage(t *testing.T, e *env, id string, at time.Time, heard bool) {
	t.Helper()
	ctx := context.Background()
	if err := e.st.AddVoicemail(ctx, store.Voicemail{ID: id, ExtensionID: 1, Caller: "+491701234567",
		ReceivedAt: at, DurationMS: 3000}); err != nil {
		t.Fatal(err)
	}
	if heard {
		e.st.SetHeard(ctx, id, true, at)
	}
	os.MkdirAll(filepath.Join(e.vmDir, "vm", "10"), 0o755)
	os.WriteFile(filepath.Join(e.vmDir, "vm", "10", id+".wav"), []byte("RIFF"), 0o644)
}

func TestMailboxMenu(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	now := time.Now()
	addMessage(t, e, "old", now.Add(-48*time.Hour), true)
	addMessage(t, e, "new", time.Date(now.Year(), now.Month(), now.Day(), 0, 5, 0, 0, now.Location()), false)

	e.fromPhone("A", "10", "*98")
	e.eng.expect(t, "answer A")
	// "Sie haben eine neue und eine alte Nachricht", main menu.
	e.eng.expect(t, "play A sound:voipd/vm-you-have,sound:voipd/vm-one,sound:voipd/vm-new-one,sound:voipd/vm-and,"+
		"sound:voipd/vm-one,sound:voipd/vm-old-one,sound:voipd/vm-main-menu")
	e.eng.dtmf("A", "1") // interrupts the menu
	e.eng.expect(t, "stop pb1")
	e.eng.played("A", "pb1")

	// The new message first: "Nachricht 1, heute um 0 Uhr 5, von 0 1 7 0 …".
	got := e.eng.next(t)
	want := "play A sound:voipd/vm-message,sound:voipd/n1,sound:voipd/vm-today,sound:voipd/vm-at,sound:voipd/n0," +
		"sound:voipd/vm-oclock,sound:voipd/n5,sound:voipd/vm-from,sound:voipd/n0,sound:voipd/n1,sound:voipd/n7," +
		"sound:voipd/n0,sound:voipd/n1,sound:voipd/n2,sound:voipd/n3,sound:voipd/n4,sound:voipd/n5,sound:voipd/n6," +
		"sound:voipd/n7,recording:vm/10/new"
	if got != want {
		t.Fatalf("message prompt:\n got %s\nwant %s", got, want)
	}
	e.eng.played("A", "pb2") // listened to the end: heard
	e.eng.expect(t, "mwi 10@voipd 0/2")
	e.eng.expect(t, "play A sound:voipd/vm-message-menu")
	e.eng.dtmf("A", "7") // delete
	e.eng.expect(t, "stop pb3")
	e.eng.played("A", "pb3")
	e.eng.expect(t, "mwi 10@voipd 0/1")
	e.eng.expect(t, "play A sound:voipd/vm-deleted")
	e.eng.played("A", "pb4")

	// The old message; skipped with 2 while it plays.
	got = e.eng.next(t)
	if !strings.HasPrefix(got, "play A sound:voipd/vm-message,sound:voipd/n2,") || !strings.HasSuffix(got, "recording:vm/10/old") {
		t.Fatalf("second message: %s", got)
	}
	e.eng.dtmf("A", "2")
	e.eng.expect(t, "stop pb5")
	e.eng.played("A", "pb5")
	e.eng.expect(t, "play A sound:voipd/vm-no-more")
	e.eng.hangup("A", engine.CauseNormal)
	e.events(t)

	if _, err := os.Stat(filepath.Join(e.vmDir, "vm", "10", "new.wav")); !os.IsNotExist(err) {
		t.Error("deleted message's file still exists")
	}
	vms, _ := e.st.Voicemails(context.Background(), nil)
	if len(vms) != 1 || vms[0].ID != "old" {
		t.Errorf("voicemails = %+v", vms)
	}
	c := e.onlyCall(t)
	if c.Callee != "*98" || c.Caller != "10" || c.Direction != "internal" || c.Status != store.CallAnswered {
		t.Errorf("call = %+v", c)
	}
}

func TestMailboxRecordGreeting(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	e.fromPhone("A", "10", "*98")
	e.eng.expect(t, "answer A")
	e.eng.expect(t, "play A sound:voipd/vm-no-messages,sound:voipd/vm-main-menu-empty")
	e.eng.played("A", "pb1")
	e.eng.dtmf("A", "0")
	e.eng.expect(t, "play A sound:voipd/vm-greet-record,sound:voipd/beep")
	e.eng.played("A", "pb2")
	name := e.eng.recordName(t, "A")
	if name != "greet/10-new" {
		t.Fatalf("greeting recording %q", name)
	}
	os.WriteFile(filepath.Join(e.vmDir, "greet", "10-new.wav"), []byte("RIFF"), 0o644)
	e.eng.recorded("A", name, 5*time.Second)
	e.eng.expect(t, "play A sound:voipd/vm-greet-review,recording:greet/10-new,sound:voipd/vm-greet-confirm")
	e.eng.played("A", "pb3")
	e.eng.dtmf("A", "1")
	e.eng.expect(t, "play A sound:voipd/vm-greet-saved")
	e.eng.played("A", "pb4")
	e.eng.hangup("A", engine.CauseNormal)
	e.events(t)

	if _, err := os.Stat(filepath.Join(e.vmDir, "greet", "10.wav")); err != nil {
		t.Errorf("greeting not kept: %v", err)
	}
	if ext, _ := e.st.ExtensionByNumber(context.Background(), "10"); !ext.VoicemailGreeting {
		t.Error("greeting flag not set")
	}
}

func TestUnknownPhoneCode(t *testing.T) {
	e := newEnv(t, 30, "hangup")
	e.fromPhone("A", "10", "*42")
	e.eng.expect(t, "answer A")
	e.eng.expect(t, "play A sound:voipd/code-unavailable")
	e.eng.played("A", "pb1")
	e.eng.expect(t, "hangup A normal")
	e.eng.hangup("A", engine.CauseNormal)
	e.events(t)
	if c := e.onlyCall(t); c.Status != store.CallFailed || c.Callee != "*42" {
		t.Errorf("call = %+v", c)
	}
}
