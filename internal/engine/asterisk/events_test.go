package asterisk

import (
	"encoding/json"
	"testing"
	"time"

	"voipd/internal/engine"
)

func TestTranslateRecordingEvents(t *testing.T) {
	for raw, want := range map[string]engine.Event{
		`{"type":"RecordingFinished","recording":{"name":"vm/10/abc","target_uri":"channel:A","state":"done","duration":4}}`:       engine.RecordingFinished{Name: "vm/10/abc", ChannelID: "A", Duration: 4 * time.Second},
		`{"type":"RecordingFailed","recording":{"name":"vm/10/abc","target_uri":"channel:A","state":"failed","cause":"no space"}}`: engine.RecordingFailed{Name: "vm/10/abc", ChannelID: "A", Cause: "no space"},
		`{"type":"PlaybackFinished","playback":{"id":"p1","target_uri":"channel:A","state":"done"}}`:                               engine.PlaybackFinished{PlaybackID: "p1", ChannelID: "A"},
	} {
		var ev ariEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		got := translateARI(ev)
		switch g := got.(type) {
		case engine.RecordingFinished:
			g.Meta = engine.Meta{}
			got = g
		case engine.RecordingFailed:
			g.Meta = engine.Meta{}
			got = g
		case engine.PlaybackFinished:
			g.Meta = engine.Meta{}
			got = g
		}
		if got != want {
			t.Errorf("%s:\n got %#v\nwant %#v", raw, got, want)
		}
	}
}
