package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var bars = []rune("▁▂▃▄▅▆▇█")

type palette struct{ green, yellow, red, dim, bold, rev, reset string }

func colors(on bool) palette {
	if !on {
		return palette{}
	}
	return palette{"\x1b[32m", "\x1b[33m", "\x1b[31m", "\x1b[2m", "\x1b[1m", "\x1b[7m", "\x1b[0m"}
}

// latency thresholds for colouring, roughly "fine / noticeable / bad"
func (p palette) forRTT(d time.Duration) string {
	switch {
	case d < 0:
		return p.red
	case d < 80*time.Millisecond:
		return p.green
	case d < 200*time.Millisecond:
		return p.yellow
	default:
		return p.red
	}
}

func fmtDur(d time.Duration) string {
	if d < 0 {
		return "-"
	}
	ms := float64(d) / float64(time.Millisecond)
	switch {
	case ms < 10:
		return strconv.FormatFloat(ms, 'f', 2, 64) + "ms"
	case ms < 1000:
		return strconv.FormatFloat(ms, 'f', 1, 64) + "ms"
	default:
		return strconv.FormatFloat(ms/1000, 'f', 2, 64) + "s"
	}
}

// sparkline over the visible samples. Scale is per row, min..max of what's
// on screen, colour is absolute (green/yellow/red by latency) so a row of
// tall green bars still reads as "fine".
func sparkline(p palette, s []Sample, width int) string {
	if len(s) > width {
		s = s[len(s)-width:]
	}
	lo, hi := time.Duration(math.MaxInt64), time.Duration(0)
	for _, x := range s {
		if x.Lost() {
			continue
		}
		if x.RTT < lo {
			lo = x.RTT
		}
		if x.RTT > hi {
			hi = x.RTT
		}
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", width-len(s)))
	prev := ""
	for _, x := range s {
		var c string
		var r rune
		if x.Lost() {
			c, r = p.red, '×'
		} else {
			c = p.forRTT(x.RTT)
			lvl := 0
			if hi > lo {
				lvl = int(float64(x.RTT-lo) / float64(hi-lo) * float64(len(bars)-1))
			}
			r = bars[lvl]
		}
		if c != prev {
			b.WriteString(c)
			prev = c
		}
		b.WriteRune(r)
	}
	b.WriteString(p.reset)
	return b.String()
}

// trunc cuts s to w runes, with an ellipsis if it had to cut
func trunc(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w > 1 {
		return string(r[:w-1]) + "…"
	}
	return string(r[:w])
}

func pad(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n > w {
		r := []rune(s)
		if w > 1 {
			return string(r[:w-1]) + "…"
		}
		return string(r[:w])
	}
	return s + strings.Repeat(" ", w-n)
}

func rpad(s string, w int) string {
	n := utf8.RuneCountInString(s)
	if n >= w {
		return s
	}
	return strings.Repeat(" ", w-n) + s
}

type row struct {
	t    Target
	snap Snapshot
}

const (
	sortNone = iota
	sortLoss
	sortLatency
	sortName
)

var sortNames = []string{"as given", "loss", "latency", "name"}

func sortRows(rows []row, mode int) {
	switch mode {
	case sortLoss:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].snap.LossPct() > rows[j].snap.LossPct() })
	case sortLatency:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].snap.Avg > rows[j].snap.Avg })
	case sortName:
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].t.Name < rows[j].t.Name })
	}
}

// Frame renders one screen. Pure function of its inputs so it's testable.
func Frame(p palette, rows []row, events []Event, width, height int, started time.Time, interval time.Duration, paused bool, sortMode int) string {
	var b strings.Builder
	nameW := 6
	for _, r := range rows {
		if n := utf8.RuneCountInString(r.t.Name); n > nameW {
			nameW = n
		}
	}
	if nameW > 24 {
		nameW = 24
	}
	// fixed columns: dot(2) name mode(9) last avg min max jit (8 each) loss(7) mos(5)
	fixed := 2 + nameW + 1 + 9 + 8*5 + 7 + 5 + 2
	histW := width - fixed
	if histW < 10 {
		histW = 10
	}

	up := time.Since(started).Truncate(time.Second)
	state := ""
	if paused {
		state = p.yellow + "  PAUSED" + p.reset
	}
	head := fmt.Sprintf("%spingwall%s  %d targets  every %s  up %s  sort: %s%s",
		p.bold, p.reset, len(rows), interval, up, sortNames[sortMode], state)
	keys := p.dim + "q quit  p pause  r reset  s sort" + p.reset
	b.WriteString(head + "\n" + keys + "\n\n")

	b.WriteString(p.dim)
	b.WriteString("  " + pad("TARGET", nameW) + " " + pad("MODE", 9) +
		rpad("LAST", 8) + rpad("AVG", 8) + rpad("MIN", 8) + rpad("MAX", 8) + rpad("JITTER", 8) +
		rpad("LOSS", 7) + rpad("MOS", 5) + "  HISTORY")
	b.WriteString(p.reset + "\n")

	for _, r := range rows {
		s := r.snap
		dot := p.green + "●" + p.reset
		switch {
		case s.Sent == 0:
			dot = p.dim + "○" + p.reset
		case s.Streak >= 3:
			dot = p.red + p.bold + "●" + p.reset
		case s.Streak > 0 || s.LossPct() > 2:
			dot = p.yellow + "●" + p.reset
		}
		loss := fmt.Sprintf("%.1f%%", s.LossPct())
		lossC := p.green
		if s.LossPct() > 0 {
			lossC = p.yellow
		}
		if s.LossPct() >= 5 {
			lossC = p.red
		}
		mos := "-"
		if s.Sent > 0 {
			mos = fmt.Sprintf("%.1f", s.MOS())
		}
		last := fmtDur(s.Last.RTT)
		if s.Sent == 0 {
			last = "…"
		} else if s.Last.Lost() && s.LastErr != "" {
			last = s.LastErr
		}
		b.WriteString(dot + " " + pad(r.t.Name, nameW) + " " + p.dim + pad(r.t.Mode(), 9) + p.reset +
			p.forRTT(s.Last.RTT) + rpad(trunc(last, 8), 8) + p.reset +
			rpad(fmtDur(nz(s.Avg, s)), 8) + rpad(fmtDur(nz(s.Min, s)), 8) + rpad(fmtDur(nz(s.Max, s)), 8) +
			rpad(fmtDur(nz(s.Jitter, s)), 8) +
			lossC + rpad(loss, 7) + p.reset + rpad(mos, 5) + "  " +
			sparkline(p, s.Samples, histW) + "\n")
	}

	// outage log fills whatever's left of the screen
	room := height - len(rows) - 7
	if room > 0 && len(events) > 0 {
		b.WriteString("\n" + p.bold + "events" + p.reset + "\n")
		start := 0
		if len(events) > room {
			start = len(events) - room
		}
		for _, e := range events[start:] {
			c := p.red
			if e.Up {
				c = p.green
			}
			b.WriteString(p.dim + e.At.Format("15:04:05") + p.reset + "  " + c + e.Text + p.reset + "\n")
		}
	}
	return b.String()
}

// show "-" for avg/min/... until we have at least one reply
func nz(d time.Duration, s Snapshot) time.Duration {
	if s.Sent-s.Lost == 0 {
		return -1
	}
	return d
}

// ---- terminal handling, without x/term ----

type tty struct {
	saved string
	f     *os.File
}

func stty(f *os.File, args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = f
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func openTTY() (*tty, error) {
	f, err := os.Open("/dev/tty")
	if err != nil {
		return nil, err
	}
	saved, err := stty(f, "-g")
	if err != nil {
		f.Close()
		return nil, err
	}
	// cbreak: keys arrive one at a time, no echo, ctrl-c still works
	if _, err := stty(f, "-icanon", "-echo", "min", "1"); err != nil {
		f.Close()
		return nil, err
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l") // alt screen, hide cursor
	return &tty{saved: saved, f: f}, nil
}

func (t *tty) Size() (w, h int) {
	out, err := stty(t.f, "size")
	if err == nil {
		if parts := strings.Fields(out); len(parts) == 2 {
			h, _ = strconv.Atoi(parts[0])
			w, _ = strconv.Atoi(parts[1])
		}
	}
	if w == 0 {
		w, h = 100, 30
	}
	return
}

func (t *tty) Restore() {
	os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
	stty(t.f, t.saved)
	t.f.Close()
}
