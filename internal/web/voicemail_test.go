package web

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voipd/internal/store"
)

// pcmWAV builds a mono 16 bit PCM WAV of the given length.
func pcmWAV(rate int, d time.Duration, channels uint16) []byte {
	n := int(d.Seconds() * float64(rate))
	data := make([]byte, n*2*int(channels))
	var b bytes.Buffer
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1))
	binary.Write(&b, binary.LittleEndian, channels)
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(rate*2*int(channels)))
	binary.Write(&b, binary.LittleEndian, uint16(2*channels))
	binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func (e *env) addVoicemail(id string, extID int64, ext string) {
	e.t.Helper()
	if err := e.st.AddVoicemail(context.Background(), store.Voicemail{ID: id, ExtensionID: extID, Caller: "+491701234567",
		ReceivedAt: time.Now(), DurationMS: 2000}); err != nil {
		e.t.Fatal(err)
	}
	dir := filepath.Join(e.vmDir, "vm", ext)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, id+".wav"), pcmWAV(8000, 2*time.Second, 1), 0o644)
}

func (c *client) raw(method, path string, body []byte, header http.Header) (*http.Response, []byte) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	req.Header.Set("X-CSRF-Token", c.csrf)
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestVoicemailAPI(t *testing.T) {
	e := newEnv(t, nil)
	e.addVoicemail("m10", 1, "10") // alice's kitchen
	e.addVoicemail("m11", 2, "11")

	// Users see their own mailboxes only.
	u, _ := e.login(alice)
	var res struct {
		Mailboxes  []mailboxJSON     `json:"mailboxes"`
		Voicemails []store.Voicemail `json:"voicemails"`
	}
	u.getJSON("/api/v1/voicemails", 200, &res)
	if len(res.Mailboxes) != 1 || res.Mailboxes[0].New != 1 || len(res.Voicemails) != 1 || res.Voicemails[0].ID != "m10" {
		t.Fatalf("user view: %+v", res)
	}
	u.send("GET", "/api/v1/voicemails/m11/audio", nil, 404)
	u.send("DELETE", "/api/v1/voicemails/m11", nil, 404)

	// Audio with Range support for the player.
	resp, body := u.raw("GET", "/api/v1/voicemails/m10/audio", nil, http.Header{"Range": {"bytes=0-3"}})
	if resp.StatusCode != http.StatusPartialContent || string(body) != "RIFF" || resp.Header.Get("Content-Type") != "audio/wav" {
		t.Errorf("range: %d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}

	u.send("PUT", "/api/v1/voicemails/m10", map[string]bool{"heard": true}, 200)
	u.getJSON("/api/v1/voicemails", 200, &res)
	if res.Mailboxes[0].New != 0 || res.Mailboxes[0].Old != 1 || !res.Voicemails[0].Heard {
		t.Errorf("after heard: %+v", res)
	}
	u.send("DELETE", "/api/v1/voicemails/m10", nil, 200)
	if _, err := os.Stat(filepath.Join(e.vmDir, "vm", "10", "m10.wav")); !os.IsNotExist(err) {
		t.Error("file not deleted")
	}

	// Admins see everything.
	a, _ := e.login(adminUser)
	a.getJSON("/api/v1/voicemails", 200, &res)
	if len(res.Mailboxes) != 2 || len(res.Voicemails) != 1 || res.Voicemails[0].Extension != "11" {
		t.Errorf("admin view: %+v", res)
	}

	var codes struct {
		Codes []map[string]string `json:"codes"`
	}
	u.getJSON("/api/v1/codes", 200, &codes)
	if len(codes.Codes) == 0 || codes.Codes[0]["code"] != "*98" {
		t.Errorf("codes = %v", codes)
	}
}

func TestGreetingUpload(t *testing.T) {
	e := newEnv(t, nil)
	u, _ := e.login(alice)
	put := func(ext string, body []byte, want int) map[string]any {
		resp, b := u.raw("PUT", "/api/v1/extensions/"+ext+"/greeting", body, http.Header{"Content-Type": {"audio/wav"}})
		if resp.StatusCode != want {
			t.Fatalf("upload = %d %s", resp.StatusCode, b)
		}
		return nil
	}
	put("1", []byte("not a wav"), 422)
	put("1", pcmWAV(44100, time.Second, 1), 422)
	put("1", pcmWAV(16000, time.Second, 2), 422)
	put("1", pcmWAV(8000, 3*time.Minute, 1), 422)
	put("2", pcmWAV(8000, time.Second, 1), 404) // not alice's

	put("1", pcmWAV(16000, 3*time.Second, 1), 200)
	if _, err := os.Stat(filepath.Join(e.vmDir, "greet", "10.wav16")); err != nil {
		t.Errorf("greeting file: %v", err)
	}
	if ext, _ := e.st.ExtensionByID(context.Background(), 1); !ext.VoicemailGreeting {
		t.Error("flag not set")
	}
	resp, _ := u.raw("GET", "/api/v1/extensions/1/greeting/audio", nil, nil)
	if resp.StatusCode != 200 {
		t.Errorf("greeting audio = %d", resp.StatusCode)
	}
	// An 8 kHz upload replaces it.
	put("1", pcmWAV(8000, 2*time.Second, 1), 200)
	if _, err := os.Stat(filepath.Join(e.vmDir, "greet", "10.wav16")); !os.IsNotExist(err) {
		t.Error("old 16 kHz greeting left behind")
	}
	u.send("DELETE", "/api/v1/extensions/1/greeting", nil, 200)
	if ext, _ := e.st.ExtensionByID(context.Background(), 1); ext.VoicemailGreeting {
		t.Error("flag still set")
	}
	resp, _ = u.raw("GET", "/api/v1/extensions/1/greeting/audio", nil, nil)
	if resp.StatusCode != 404 {
		t.Errorf("greeting audio after delete = %d", resp.StatusCode)
	}
}
