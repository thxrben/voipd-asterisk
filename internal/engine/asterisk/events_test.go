package asterisk

import (
	"encoding/json"
	"net/netip"
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

func TestTranslateSecurityEvents(t *testing.T) {
	e := &Engine{}
	for _, tc := range []struct {
		msg  Message
		want *engine.AuthFailure
	}{
		{Message{"Event": "ChallengeResponseFailed", "Service": "PJSIP", "AccountID": "kitchen",
			"RemoteAddress": "IPV4/UDP/192.0.2.10/5060"},
			&engine.AuthFailure{IP: netip.MustParseAddr("192.0.2.10"), Account: "kitchen", Kind: "password"}},
		{Message{"Event": "InvalidAccountID", "Service": "PJSIP", "AccountID": "admin",
			"RemoteAddress": "IPV6/UDP/2001:db8::5/5060"},
			&engine.AuthFailure{IP: netip.MustParseAddr("2001:db8::5"), Account: "admin", Kind: "unknown_account"}},
		{Message{"Event": "FailedACL", "Service": "PJSIP", "AccountID": "kitchen",
			"RemoteAddress": "IPV4/TCP/198.51.100.3/40000"},
			&engine.AuthFailure{IP: netip.MustParseAddr("198.51.100.3"), Account: "kitchen", Kind: "acl"}},
		{Message{"Event": "InvalidPassword", "Service": "AMI", "RemoteAddress": "IPV4/TCP/127.0.0.1/1"}, nil},
		{Message{"Event": "ChallengeResponseFailed", "Service": "PJSIP", "AccountID": "<unknown>",
			"RemoteAddress": "IPV4/UDP/192.0.2.10/5060"}, nil},
		{Message{"Event": "InvalidPassword", "Service": "PJSIP", "RemoteAddress": "garbage"}, nil},
	} {
		got := e.translateAMI(tc.msg)
		if tc.want == nil {
			if got != nil {
				t.Errorf("%v: got %#v", tc.msg, got)
			}
			continue
		}
		f, ok := got.(engine.AuthFailure)
		f.Meta = engine.Meta{}
		if !ok || f != *tc.want {
			t.Errorf("%v:\n got %#v\nwant %#v", tc.msg, got, *tc.want)
		}
	}
}
