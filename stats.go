package main

import (
	"math"
	"sync"
	"time"
)

// Sample is one probe result. RTT < 0 means lost.
type Sample struct {
	At  time.Time
	RTT time.Duration
}

func (s Sample) Lost() bool { return s.RTT < 0 }

// Series keeps the last N samples in a ring plus running totals since start
// (or since the last reset).
type Series struct {
	mu      sync.Mutex
	ring    []Sample
	next    int
	full    bool
	sent    int
	lost    int
	min     time.Duration
	max     time.Duration
	sum     time.Duration
	jitSum  time.Duration // sum of |rtt[i] - rtt[i-1]|, RFC 3550 style but not smoothed
	jitN    int
	last    time.Duration
	hasLast bool
	streak  int // consecutive losses right now
	lastErr string
}

func NewSeries(n int) *Series {
	return &Series{ring: make([]Sample, n)}
}

func (s *Series) Add(sm Sample, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ring[s.next] = sm
	s.next = (s.next + 1) % len(s.ring)
	if s.next == 0 {
		s.full = true
	}
	s.sent++
	if sm.Lost() {
		s.lost++
		s.streak++
		if err != nil {
			s.lastErr = err.Error()
		}
		return
	}
	s.streak = 0
	s.lastErr = ""
	if s.sent-s.lost == 1 || sm.RTT < s.min {
		s.min = sm.RTT
	}
	if sm.RTT > s.max {
		s.max = sm.RTT
	}
	s.sum += sm.RTT
	if s.hasLast {
		d := sm.RTT - s.last
		if d < 0 {
			d = -d
		}
		s.jitSum += d
		s.jitN++
	}
	s.last = sm.RTT
	s.hasLast = true
}

func (s *Series) Reset() {
	s.mu.Lock()
	n := len(s.ring)
	s.mu.Unlock()
	*s = Series{ring: make([]Sample, n)}
}

// Snapshot is a copy that the UI can read without holding the lock.
type Snapshot struct {
	Samples []Sample // oldest first
	Sent    int
	Lost    int
	Min     time.Duration
	Max     time.Duration
	Avg     time.Duration
	Jitter  time.Duration
	Last    Sample
	Streak  int
	LastErr string
}

func (s *Series) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Sample
	if s.full {
		out = append(out, s.ring[s.next:]...)
	}
	out = append(out, s.ring[:s.next]...)

	snap := Snapshot{Samples: out, Sent: s.sent, Lost: s.lost, Min: s.min, Max: s.max,
		Streak: s.streak, LastErr: s.lastErr}
	if ok := s.sent - s.lost; ok > 0 {
		snap.Avg = s.sum / time.Duration(ok)
	}
	if s.jitN > 0 {
		snap.Jitter = s.jitSum / time.Duration(s.jitN)
	}
	if len(out) > 0 {
		snap.Last = out[len(out)-1]
	}
	return snap
}

func (sn Snapshot) LossPct() float64 {
	if sn.Sent == 0 {
		return 0
	}
	return float64(sn.Lost) / float64(sn.Sent) * 100
}

// MOS estimate from latency, jitter and loss - the simplified E-model
// that VoIP monitoring tools use. Good enough to answer "will calls
// sound bad on this link" at a glance.
func (sn Snapshot) MOS() float64 {
	if sn.Sent-sn.Lost == 0 {
		return 1
	}
	lat := float64(sn.Avg.Milliseconds()) + 2*float64(sn.Jitter.Milliseconds()) + 10
	var r float64
	if lat < 160 {
		r = 93.2 - lat/40
	} else {
		r = 93.2 - (lat-120)/10
	}
	r -= 2.5 * sn.LossPct()
	if r < 0 {
		r = 0
	}
	mos := 1 + 0.035*r + 0.000007*r*(r-60)*(100-r)
	return math.Max(1, math.Min(4.5, mos))
}
