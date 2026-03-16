//go:build !windows

// udp_sockopt_unix.go – Linux / macOS / other Unix implementation of setBroadcast.
//
// On POSIX systems the raw file descriptor is a plain int.  We use the
// golang.org/x/sys-free path via SyscallConn + Control so there are no
// extra module dependencies beyond the standard library.

package network

import (
	"fmt"
	"net"
	"syscall"
)

// setBroadcast enables SO_BROADCAST on conn so that WriteToUDP calls
// targeting subnet broadcast addresses (e.g. 192.168.1.255) and the
// global broadcast address (255.255.255.255) succeed without EACCES.
func setBroadcast(conn *net.UDPConn) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("setBroadcast: SyscallConn: %w", err)
	}

	var setSockOptErr error
	controlErr := rawConn.Control(func(fd uintptr) {
		setSockOptErr = syscall.SetsockoptInt(
			int(fd),
			syscall.SOL_SOCKET,
			syscall.SO_BROADCAST,
			1,
		)
	})
	if controlErr != nil {
		return fmt.Errorf("setBroadcast: Control: %w", controlErr)
	}
	if setSockOptErr != nil {
		return fmt.Errorf("setBroadcast: setsockopt SO_BROADCAST: %w", setSockOptErr)
	}
	return nil
}
