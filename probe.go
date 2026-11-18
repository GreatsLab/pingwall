package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Prober sends one probe and reports the round trip.
type Prober interface {
	Probe(ctx context.Context) (time.Duration, error)
	Close() error
}

// Target is what the user asked for, parsed.
type Target struct {
	Name string // label shown in the UI
	Host string
	Port string // "" for icmp
}

func (t Target) Mode() string {
	if t.Port != "" {
		return "tcp/" + t.Port
	}
	return "icmp"
}

// ParseTarget accepts:
//
//	8.8.8.8
//	gw=192.168.1.1          (name=host)
//	tcp://example.com:443   (tcp connect time)
//	example.com:443         (same)
func ParseTarget(s string) (Target, error) {
	s = strings.TrimSpace(s)
	var t Target
	if i := strings.Index(s, "="); i > 0 {
		t.Name, s = s[:i], s[i+1:]
	}
	s = strings.TrimPrefix(s, "icmp://")
	tcp := strings.HasPrefix(s, "tcp://")
	s = strings.TrimPrefix(s, "tcp://")
	if host, port, err := net.SplitHostPort(s); err == nil {
		t.Host, t.Port = host, port
	} else if tcp {
		return t, fmt.Errorf("%q: tcp target needs a port, like tcp://host:443", s)
	} else {
		t.Host = strings.Trim(s, "[]")
	}
	if t.Host == "" {
		return t, errors.New("empty target")
	}
	if t.Name == "" {
		t.Name = t.Host
		if t.Port != "" {
			t.Name += ":" + t.Port
		}
	}
	return t, nil
}

// ---- tcp ----

type tcpProber struct{ addr string }

func (p *tcpProber) Probe(ctx context.Context) (time.Duration, error) {
	var d net.Dialer
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		// a RST still proves the host is up and gives us an RTT
		var op *net.OpError
		if errors.As(err, &op) && strings.Contains(err.Error(), "connection refused") {
			return time.Since(start), nil
		}
		return -1, shortErr(err)
	}
	rtt := time.Since(start)
	c.Close()
	return rtt, nil
}

func (p *tcpProber) Close() error { return nil }

// ---- icmp ----

// icmpProber builds echo requests by hand. On Linux and macOS it uses an
// unprivileged ICMP datagram socket (no root needed on Linux if
// net.ipv4.ping_group_range includes your gid, always on macOS); if that's
// not allowed it falls back to a raw socket, which needs root/CAP_NET_RAW.
type icmpProber struct {
	conn net.PacketConn
	dst  net.Addr
	raw  bool
	id   uint16
	seq  uint16
	ip   net.IP
	rbuf []byte
}

func newICMPProber(host string) (*icmpProber, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, shortErr(err)
	}
	var ip net.IP
	for _, cand := range ips {
		if v4 := cand.To4(); v4 != nil {
			ip = v4
			break
		}
	}
	if ip == nil {
		return nil, fmt.Errorf("%s: no IPv4 address (IPv6 icmp not supported yet, use tcp://)", host)
	}
	p := &icmpProber{ip: ip, id: uint16(os.Getpid() & 0xffff), rbuf: make([]byte, 1500)}

	if c, err := listenICMPDgram(); err == nil {
		p.conn = c
		p.dst = &net.UDPAddr{IP: ip}
	} else if c, err2 := net.ListenPacket("ip4:icmp", "0.0.0.0"); err2 == nil {
		p.conn = c
		p.raw = true
		p.dst = &net.IPAddr{IP: ip}
	} else {
		return nil, fmt.Errorf("can't open an ICMP socket (%v). Run as root, or on Linux: sudo sysctl -w net.ipv4.ping_group_range=\"0 2147483647\". Or use tcp:// targets", err)
	}
	return p, nil
}

func (p *icmpProber) Probe(ctx context.Context) (time.Duration, error) {
	p.seq++
	seq := p.seq
	msg := echoRequest(p.id, seq, []byte("pingwall"))
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(2 * time.Second)
	}
	start := time.Now()
	if _, err := p.conn.WriteTo(msg, p.dst); err != nil {
		return -1, shortErr(err)
	}
	p.conn.SetReadDeadline(deadline)
	for {
		n, from, err := p.conn.ReadFrom(p.rbuf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return -1, errors.New("timeout")
			}
			return -1, shortErr(err)
		}
		if !sameIP(from, p.ip) {
			continue
		}
		b := p.rbuf[:n]
		// raw sockets on Linux hand us the ICMP part only for ip4:icmp, but
		// macOS includes the IP header. Strip it if it's there.
		if len(b) >= 20 && b[0]>>4 == 4 {
			b = b[int(b[0]&0x0f)*4:]
		}
		typ, id, rseq, ok := parseEcho(b)
		if !ok {
			continue
		}
		switch typ {
		case 0: // echo reply
			// with dgram sockets the kernel rewrites the id, so only check it on raw
			if rseq != seq || (p.raw && id != p.id) {
				continue // late reply from an earlier probe
			}
			return time.Since(start), nil
		case 3:
			return -1, errors.New("unreachable")
		case 11:
			return -1, errors.New("ttl exceeded")
		}
	}
}

func (p *icmpProber) Close() error { return p.conn.Close() }

func sameIP(a net.Addr, ip net.IP) bool {
	switch v := a.(type) {
	case *net.UDPAddr:
		return v.IP.Equal(ip)
	case *net.IPAddr:
		return v.IP.Equal(ip)
	}
	return false
}

func echoRequest(id, seq uint16, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = 8 // type: echo request
	b[1] = 0 // code
	binary.BigEndian.PutUint16(b[4:], id)
	binary.BigEndian.PutUint16(b[6:], seq)
	copy(b[8:], payload)
	binary.BigEndian.PutUint16(b[2:], checksum(b))
	return b
}

func parseEcho(b []byte) (typ uint8, id, seq uint16, ok bool) {
	if len(b) < 8 {
		return 0, 0, 0, false
	}
	typ = b[0]
	if typ == 3 || typ == 11 {
		// error messages carry the original header 8 bytes + IP header in
		return typ, 0, 0, true
	}
	return typ, binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:]), typ == 0
}

// RFC 1071 internet checksum
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

func NewProber(t Target) (Prober, error) {
	if t.Port != "" {
		return &tcpProber{addr: net.JoinHostPort(t.Host, t.Port)}, nil
	}
	p, err := newICMPProber(t.Host)
	if err != nil {
		// return a real nil interface, not a nil *icmpProber wrapped in one -
		// callers check prober == nil
		return nil, err
	}
	return p, nil
}

func shortErr(err error) error {
	s := err.Error()
	for _, k := range []string{"no such host", "connection refused", "network is unreachable", "no route to host", "i/o timeout", "permission denied"} {
		if strings.Contains(s, k) {
			if k == "i/o timeout" {
				k = "timeout"
			}
			return errors.New(k)
		}
	}
	return err
}
