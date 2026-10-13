package callflow

import (
	"math"
	"strconv"
	"strings"
)

// qualityTracker collects codec/transport details once and RTCP stats
// periodically for the two legs of a bridged call.
type qualityTracker struct {
	c    *call
	legs []*leg
	last map[string]rtcpStats
}

type rtcpStats struct {
	rxCount, txCount int64
	lost, remoteLost int64
	rxJitter         float64 // seconds
	rtt              float64 // seconds
}

func newQualityTracker(c *call, legs ...*leg) *qualityTracker {
	return &qualityTracker{c: c, legs: legs, last: map[string]rtcpStats{}}
}

func (q *qualityTracker) get(l *leg, expr string) (string, bool) {
	ctx, cancel := q.c.actx()
	defer cancel()
	v, err := q.c.d.Engine.ChannelVar(ctx, l.id, expr)
	return v, err == nil
}

// media reads codec, transport security and remote address of each leg.
func (q *qualityTracker) media() {
	for _, l := range q.legs {
		if v, ok := q.get(l, "CHANNEL(audionativeformat)"); ok {
			l.rec.Quality.Codec = strings.Trim(v, "()")
		}
		if v, ok := q.get(l, "CHANNEL(pjsip,secure)"); ok && v == "1" {
			l.rec.Quality.Transport = "tls"
		}
		if v, ok := q.get(l, "CHANNEL(rtp,secure,audio)"); ok {
			l.rec.Quality.SRTP = v == "1"
		}
		if v, ok := q.get(l, "CHANNEL(pjsip,remote_addr)"); ok {
			l.rec.Quality.RemoteAddr = v
		}
	}
}

// poll samples RTCP stats of every leg that is still up.
func (q *qualityTracker) poll() {
	for _, l := range q.legs {
		if l.state == legGone {
			continue
		}
		v, ok := q.get(l, "CHANNEL(rtcp,all)")
		if !ok || v == "" {
			continue
		}
		// Counters are cumulative; a smaller sample means the RTP session
		// is already being torn down, so keep the earlier one.
		if s, ok := parseRTCP(v); ok && s.rxCount+s.txCount >= q.last[l.id].rxCount+q.last[l.id].txCount {
			q.last[l.id] = s
		}
	}
}

// finish copies the last sample of each leg into its record.
func (q *qualityTracker) finish() {
	for _, l := range q.legs {
		s, ok := q.last[l.id]
		if !ok {
			q.c.saveLeg(l)
			continue
		}
		qq := &l.rec.Quality
		qq.RxPackets, qq.TxPackets = &s.rxCount, &s.txCount
		qq.RxLost, qq.TxLost = &s.lost, &s.remoteLost
		jit, rtt := s.rxJitter*1000, s.rtt*1000
		qq.JitterMS, qq.RTTMS = &jit, &rtt
		lossPct := 0.0
		if total := s.rxCount + s.lost; total > 0 {
			lossPct = float64(s.lost) / float64(total) * 100
		}
		mos := estimateMOS(lossPct, jit, rtt)
		qq.MOS = &mos
		q.c.saveLeg(l)
	}
}

// parseRTCP parses Asterisk's CHANNEL(rtcp,all) output:
// "ssrc=…;themssrc=…;lp=0;rxjitter=0.000125;rxcount=503;txjitter=…;txcount=502;rlp=0;rtt=0.000000;…"
func parseRTCP(s string) (rtcpStats, bool) {
	var r rtcpStats
	found := false
	for _, kv := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		i, _ := strconv.ParseInt(v, 10, 64)
		f, _ := strconv.ParseFloat(v, 64)
		switch k {
		case "rxcount":
			r.rxCount, found = i, true
		case "txcount":
			r.txCount = i
		case "lp":
			r.lost = i
		case "rlp":
			r.remoteLost = i
		case "rxjitter":
			r.rxJitter = f
		case "rtt":
			r.rtt = f
		}
	}
	return r, found
}

// estimateMOS implements the ITU-T G.107 E-model with default values for
// everything but delay and loss:
//
//	Ta     = one-way delay: RTT/2 + jitter buffer (≈2×jitter) + 10 ms codec
//	Id     = 0.024·Ta + 0.11·(Ta−177.3)·H(Ta−177.3)
//	Ie,eff = 95·Ppl / (Ppl + Bpl), Bpl = 25.1 (G.711 with packet loss concealment)
//	R      = 93.2 − Id − Ie,eff, mapped to MOS 1..4.5
func estimateMOS(lossPct, jitterMS, rttMS float64) float64 {
	ta := rttMS/2 + 2*jitterMS + 10
	id := 0.024 * ta
	if ta > 177.3 {
		id += 0.11 * (ta - 177.3)
	}
	ie := 95 * lossPct / (lossPct + 25.1)
	r := math.Max(0, math.Min(100, 93.2-id-ie))
	mos := 1 + 0.035*r + 7e-6*r*(r-60)*(100-r)
	return math.Round(math.Max(1, math.Min(4.5, mos))*100) / 100
}
