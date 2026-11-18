package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestChecksum(t *testing.T) {
	// echo request id=1 seq=1 no payload: 08 00 [f7 fd] 00 01 00 01
	b := echoRequest(1, 1, nil)
	if b[2] != 0xf7 || b[3] != 0xfd {
		t.Fatalf("checksum = %02x%02x, want f7fd", b[2], b[3])
	}
	// checksum over a packet including its checksum is 0
	if checksum(b) != 0 {
		t.Fatal("checksum doesn't verify")
	}
	// odd length payload
	if checksum(echoRequest(7, 9, []byte("abc"))) != 0 {
		t.Fatal("odd-length checksum doesn't verify")
	}
}

func TestParseEcho(t *testing.T) {
	b := echoRequest(0x1234, 42, []byte("x"))
	b[0] = 0 // turn it into a reply
	typ, id, seq, ok := parseEcho(b)
	if !ok || typ != 0 || id != 0x1234 || seq != 42 {
		t.Fatal(typ, id, seq, ok)
	}
}

func TestParseTarget(t *testing.T) {
	cases := []struct{ in, name, host, port string }{
		{"8.8.8.8", "8.8.8.8", "8.8.8.8", ""},
		{"gw=192.168.1.1", "gw", "192.168.1.1", ""},
		{"tcp://example.com:443", "example.com:443", "example.com", "443"},
		{"web=example.com:80", "web", "example.com", "80"},
		{"[2001:db8::1]:443", "2001:db8::1:443", "2001:db8::1", "443"},
	}
	for _, c := range cases {
		got, err := ParseTarget(c.in)
		if err != nil {
			t.Fatal(c.in, err)
		}
		if got.Name != c.name || got.Host != c.host || got.Port != c.port {
			t.Errorf("%s: got %+v", c.in, got)
		}
	}
	if _, err := ParseTarget("tcp://nohost"); err == nil {
		t.Error("tcp without port should fail")
	}
}

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

func TestSeries(t *testing.T) {
	s := NewSeries(4)
	for _, v := range []float64{10, 20, -1, 30, 10, 20} {
		var err error
		if v < 0 {
			err = errors.New("timeout")
		}
		s.Add(Sample{At: time.Now(), RTT: ms(v)}, err)
	}
	sn := s.Snapshot()
	if sn.Sent != 6 || sn.Lost != 1 {
		t.Fatal(sn.Sent, sn.Lost)
	}
	if sn.Min != ms(10) || sn.Max != ms(30) || sn.Avg != ms(18) {
		t.Fatal(sn.Min, sn.Max, sn.Avg)
	}
	// jitter: |20-10| |30-20| |10-30| |20-10| = 10,10,20,10 -> 12.5
	if sn.Jitter != ms(12.5) {
		t.Fatal(sn.Jitter)
	}
	if len(sn.Samples) != 4 || sn.Samples[3].RTT != ms(20) || !sn.Samples[0].Lost() {
		t.Fatal("ring order wrong", sn.Samples)
	}
}

func TestMOS(t *testing.T) {
	good := Snapshot{Sent: 100, Avg: ms(20), Jitter: ms(2)}
	bad := Snapshot{Sent: 100, Lost: 15, Avg: ms(350), Jitter: ms(80)}
	if g := good.MOS(); g < 4.2 {
		t.Errorf("good link MOS %.2f", g)
	}
	if b := bad.MOS(); b > 2.5 {
		t.Errorf("bad link MOS %.2f", b)
	}
}

func TestFrameFitsWidth(t *testing.T) {
	s := NewSeries(500)
	for i := 0; i < 500; i++ {
		s.Add(Sample{RTT: ms(float64(i % 50))}, nil)
	}
	rows := []row{{t: Target{Name: "a-very-long-target-name-indeed", Host: "x"}, snap: s.Snapshot()}}
	out := Frame(colors(false), rows, nil, 120, 30, time.Now(), time.Second, false, 0)
	for _, line := range strings.Split(out, "\n") {
		if n := len([]rune(line)); n > 120 {
			t.Errorf("line is %d wide: %q", n, line)
		}
	}
}

func TestPad(t *testing.T) {
	if pad("tcp/8803", 8) != "tcp/8803" {
		t.Error("exact width must not be truncated")
	}
	if trunc("blackhole-long", 8) != "blackho…" {
		t.Error(trunc("blackhole-long", 8))
	}
}
