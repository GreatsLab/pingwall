//go:build linux || darwin

package main

import (
	"net"
	"os"
	"syscall"
)

// listenICMPDgram opens SOCK_DGRAM/IPPROTO_ICMP, the "ping socket".
// The stdlib has no name for it, so we build it from a raw fd.
func listenICMPDgram() (net.PacketConn, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		return nil, err
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{}); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "icmp")
	defer f.Close() // FilePacketConn dups the fd
	return net.FilePacketConn(f)
}
