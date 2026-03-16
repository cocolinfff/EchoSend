// udp_helpers.go – platform-agnostic helpers referenced by udp.go
//
// This file isolates two small dependencies that would otherwise scatter
// import noise across the main udp.go file:
//
//  1. cryptoRandRead – a thin alias for crypto/rand.Read so that udp.go
//     does not need to import "crypto/rand" directly (keeping the hot-path
//     file focused on network logic).
//
//  2. pseudoCounter – an atomic uint64 used as a monotonic fallback when
//     crypto/rand.Read fails (an extremely rare OS-level condition).  The
//     counter is package-scoped so all UDPEngine instances on the same
//     process share the same sequence, guaranteeing uniqueness within the
//     process even if multiple engines are created during tests.

package network

import (
	"crypto/rand"
	"sync/atomic"
)

// cryptoRandRead fills b with cryptographically random bytes.
// It is a direct delegate to crypto/rand.Read, exposed as a package-level
// variable so tests can substitute a deterministic implementation without
// build tags or interface indirection.
var cryptoRandRead = func(b []byte) (int, error) {
	return rand.Read(b)
}

// pseudoCounter is incremented atomically each time cryptoRandRead fails and
// the fallback timestamp+counter packet-ID path is taken.  Under normal
// operating conditions this counter is never touched.
var pseudoCounter atomic.Uint64
