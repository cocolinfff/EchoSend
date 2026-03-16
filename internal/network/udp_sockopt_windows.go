//go:build windows

// udp_sockopt_windows.go – Windows implementation of setBroadcast.
//
// On Windows the raw file descriptor exposed by SyscallConn is a SOCKET
// handle, typed as syscall.Handle (uintptr alias).  We cast it explicitly
// so the code compiles on both 32-bit and 64-bit Windows targets.

package network

import (
	"fmt"
	"net"
	"syscall"
)

// setBroadcast enables SO_BROADCAST on conn so that WriteToUDP calls
// targeting subnet and global broadcast addresses succeed.
func setBroadcast(conn *net.UDPConn) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("setBroadcast: SyscallConn: %w", err)
	}

	var setSockOptErr error
	controlErr := rawConn.Control(func(fd uintptr) {
		setSockOptErr = syscall.SetsockoptInt(
			syscall.Handle(fd),
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
