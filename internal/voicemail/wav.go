package voicemail

import (
	"encoding/binary"
	"errors"
	"time"
)

// MaxGreeting is the longest greeting accepted.
const MaxGreeting = 2 * time.Minute

const maxGreetingBytes = 16000*2*120 + 4096

// WAVInfo describes a PCM WAV file.
type WAVInfo struct {
	SampleRate int
	Duration   time.Duration
}

// ParseWAV checks that data is a WAV file Asterisk can play as a greeting:
// PCM, mono, 16 bit, 8 or 16 kHz.
func ParseWAV(data []byte) (WAVInfo, error) {
	var info WAVInfo
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return info, errors.New("not a WAV file; please upload a WAV (PCM, mono, 16 bit, 8 or 16 kHz)")
	}
	var haveFmt bool
	var dataLen int
	for p := 12; p+8 <= len(data); {
		id, size := string(data[p:p+4]), int(binary.LittleEndian.Uint32(data[p+4:p+8]))
		body := p + 8
		if body+size > len(data) {
			size = len(data) - body // tolerate a truncated last chunk
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return info, errors.New("broken WAV header")
			}
			format := binary.LittleEndian.Uint16(data[body:])
			channels := binary.LittleEndian.Uint16(data[body+2:])
			rate := int(binary.LittleEndian.Uint32(data[body+4:]))
			bits := binary.LittleEndian.Uint16(data[body+14:])
			if format != 1 || bits != 16 {
				return info, errors.New("the WAV must be uncompressed PCM with 16 bit samples")
			}
			if channels != 1 {
				return info, errors.New("the WAV must be mono")
			}
			if rate != 8000 && rate != 16000 {
				return info, errors.New("the WAV must have 8 kHz or 16 kHz sample rate")
			}
			info.SampleRate, haveFmt = rate, true
		case "data":
			dataLen = size
		}
		p = body + size + size%2
	}
	if !haveFmt || dataLen == 0 {
		return info, errors.New("the WAV contains no audio")
	}
	info.Duration = time.Duration(dataLen/2) * time.Second / time.Duration(info.SampleRate)
	return info, nil
}
