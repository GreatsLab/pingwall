// pingwall - watch latency, jitter and loss to many hosts at once, in one terminal.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "0.4.0"

// Event is an outage starting or ending.
type Event struct {
	At   time.Time
	Text string
	Up   bool
}

type monitor struct {
	t      Target
	prober Prober
	series *Series
	err    error // setup error (e.g. DNS failed)
	down   time.Time
}

func usage() {
	fmt.Fprintf(os.Stderr, `pingwall %s - latency / jitter / loss for many hosts, live.

usage: pingwall [flags] target [target...]

targets:
  8.8.8.8                 icmp echo
  gw=192.168.1.1          give it a name
  tcp://example.com:443   tcp connect time (works where icmp is blocked)
  example.com:22          same, port implies tcp

flags:
`, version)
	flag.PrintDefaults()
	fmt.Fprintln(os.Stderr, `
examples:
  pingwall gw=192.168.1.1 cloudflare=1.1.1.1 google=8.8.8.8
  pingwall -f targets.txt -log isp.csv
  pingwall -c 20 1.1.1.1 8.8.8.8         # 20 probes each, print a report, exit`)
}

func main() {
	interval := flag.Duration("i", time.Second, "probe interval per target")
	timeout := flag.Duration("t", time.Second, "probe timeout")
	file := flag.String("f", "", "read targets from file (one per line, # comments)")
	count := flag.Int("c", 0, "send this many probes per target, print a report and exit (no UI)")
	window := flag.Int("w", 300, "samples to keep per target for the history graph")
	logPath := flag.String("log", "", "append every sample to this CSV file")
	downAfter := flag.Int("down", 3, "consecutive losses before a target counts as down")
	noColor := flag.Bool("no-color", false, "disable colours")
	showVer := flag.Bool("version", false, "print version")
	flag.Usage = usage
	flag.Parse()

	if *showVer {
		fmt.Println("pingwall", version)
		return
	}

	specs := flag.Args()
	if *file != "" {
		more, err := readTargets(*file)
		if err != nil {
			fatal(err)
		}
		specs = append(specs, more...)
	}
	if len(specs) == 0 {
		usage()
		os.Exit(2)
	}

	var mons []*monitor
	for _, s := range specs {
		t, err := ParseTarget(s)
		if err != nil {
			fatal(err)
		}
		m := &monitor{t: t, series: NewSeries(*window)}
		p, err := NewProber(t)
		if err != nil {
			m.err = err
		} else {
			m.prober = p
		}
		mons = append(mons, m)
	}

	var logw *csv.Writer
	var logMu sync.Mutex
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			fatal(err)
		}
		defer f.Close()
		st, _ := f.Stat()
		logw = csv.NewWriter(f)
		if st.Size() == 0 {
			logw.Write([]string{"time", "target", "host", "mode", "rtt_ms", "error"})
		}
		defer logw.Flush()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	var paused sync.Mutex // held while paused, probes block on it
	var evMu sync.Mutex
	var events []Event
	addEvent := func(e Event) { evMu.Lock(); events = append(events, e); evMu.Unlock() }

	var wg sync.WaitGroup
	start := time.Now()
	for i, m := range mons {
		wg.Add(1)
		go func(m *monitor, offset time.Duration) {
			defer wg.Done()
			defer func() {
				if m.prober != nil {
					m.prober.Close()
				}
			}()
			// spread probes out so we don't burst N packets at the same instant
			select {
			case <-time.After(offset):
			case <-ctx.Done():
				return
			}
			tick := time.NewTicker(*interval)
			defer tick.Stop()
			for n := 0; *count == 0 || n < *count; n++ {
				paused.Lock()
				paused.Unlock()
				var rtt time.Duration
				var err error
				if m.prober == nil {
					// setup failed (DNS down, no route...). Keep retrying every
					// interval and count it as loss, DNS outages are outages too.
					m.prober, m.err = NewProber(m.t)
				}
				if m.prober == nil {
					rtt, err = -1, m.err
				} else {
					pctx, pcancel := context.WithTimeout(ctx, *timeout)
					rtt, err = m.prober.Probe(pctx)
					pcancel()
				}
				if ctx.Err() != nil {
					return
				}
				now := time.Now()
				before := m.series.Snapshot().Streak
				m.series.Add(Sample{At: now, RTT: rtt}, err)
				after := m.series.Snapshot().Streak

				if before < *downAfter && after >= *downAfter {
					m.down = now.Add(-time.Duration(*downAfter-1) * *interval)
					addEvent(Event{At: now, Text: fmt.Sprintf("%s DOWN (%s)", m.t.Name, errText(err))})
				} else if before >= *downAfter && after == 0 {
					addEvent(Event{At: now, Up: true, Text: fmt.Sprintf("%s UP after %s", m.t.Name, now.Sub(m.down).Truncate(time.Second))})
				}

				if logw != nil {
					logMu.Lock()
					ms, e := "", ""
					if err != nil {
						e = err.Error()
					} else {
						ms = fmt.Sprintf("%.3f", float64(rtt)/float64(time.Millisecond))
					}
					logw.Write([]string{now.Format(time.RFC3339Nano), m.t.Name, m.t.Host, m.t.Mode(), ms, e})
					logw.Flush()
					logMu.Unlock()
				}
				if *count != 0 && n == *count-1 {
					return
				}
				select {
				case <-tick.C:
				case <-ctx.Done():
					return
				}
			}
		}(m, time.Duration(i)**interval/time.Duration(len(mons)))
	}

	color := !*noColor && os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)

	if *count > 0 {
		wg.Wait()
		printReport(colors(color), mons)
		return
	}

	term, err := openTTY()
	if err != nil {
		cancel()
		fatal(fmt.Errorf("need a terminal for the live view (%v). Use -c N for a one-shot report", err))
	}
	defer term.Restore()

	keys := make(chan byte)
	go func() {
		r := bufio.NewReader(term.f)
		for {
			b, err := r.ReadByte()
			if err != nil {
				return
			}
			keys <- b
		}
	}()

	isPaused := false
	sortMode := sortNone
	pal := colors(color)
	redraw := time.NewTicker(250 * time.Millisecond)
	defer redraw.Stop()
	for {
		w, h := term.Size()
		rows := make([]row, len(mons))
		for i, m := range mons {
			rows[i] = row{t: m.t, snap: m.series.Snapshot()}
		}
		sortRows(rows, sortMode)
		evMu.Lock()
		ev := append([]Event(nil), events...)
		evMu.Unlock()
		frame := Frame(pal, rows, ev, w, h, start, *interval, isPaused, sortMode)
		// clear each line as we go instead of the whole screen, no flicker
		os.Stdout.WriteString("\x1b[H" + strings.ReplaceAll(frame, "\n", "\x1b[K\n") + "\x1b[J")

		select {
		case <-ctx.Done():
			return
		case k := <-keys:
			switch k {
			case 'q', 'Q', 3:
				cancel()
				return
			case 'p', 'P', ' ':
				if isPaused {
					paused.Unlock()
				} else {
					paused.Lock()
				}
				isPaused = !isPaused
			case 'r', 'R':
				for _, m := range mons {
					m.series.Reset()
				}
				evMu.Lock()
				events = nil
				evMu.Unlock()
				start = time.Now()
			case 's', 'S':
				sortMode = (sortMode + 1) % len(sortNames)
			}
		case <-redraw.C:
		}
	}
}

func errText(err error) string {
	if err == nil {
		return "lost"
	}
	return err.Error()
}

func printReport(p palette, mons []*monitor) {
	nameW := 6
	for _, m := range mons {
		if len(m.t.Name) > nameW {
			nameW = len(m.t.Name)
		}
	}
	fmt.Printf("%s%-*s  %-8s %5s %5s %6s %9s %9s %9s %9s %5s%s\n", p.bold, nameW, "TARGET", "MODE",
		"SENT", "LOST", "LOSS", "MIN", "AVG", "MAX", "JITTER", "MOS", p.reset)
	for _, m := range mons {
		s := m.series.Snapshot()
		if s.Sent == s.Lost && s.LastErr != "" {
			fmt.Printf("%-*s  %-8s %5d %5d %5.1f%%  %s%s%s\n", nameW, m.t.Name, m.t.Mode(),
				s.Sent, s.Lost, s.LossPct(), p.red, s.LastErr, p.reset)
			continue
		}
		fmt.Printf("%-*s  %-8s %5d %5d %5.1f%% %9s %9s %9s %9s %5.1f\n", nameW, m.t.Name, m.t.Mode(),
			s.Sent, s.Lost, s.LossPct(), fmtDur(nz(s.Min, s)), fmtDur(nz(s.Avg, s)), fmtDur(nz(s.Max, s)),
			fmtDur(nz(s.Jitter, s)), s.MOS())
	}
}

func readTargets(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "name host" (space separated) is accepted too, people write it that way
		if parts := strings.Fields(line); len(parts) == 2 && !strings.Contains(parts[0], "=") {
			line = parts[0] + "=" + parts[1]
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "pingwall:", err)
	os.Exit(1)
}
