//go:build !linux && !darwin

package main

import (
	"errors"
	"net"
)

func listenICMPDgram() (net.PacketConn, error) {
	return nil, errors.New("unprivileged icmp not supported on this OS")
}
