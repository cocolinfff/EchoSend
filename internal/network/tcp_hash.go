// tcp_hash.go – SHA-256 factory implementation for the TCP file transfer layer.
//
// This file is intentionally separate from tcp.go to keep the crypto import
// out of the main networking file and to make the factory function easy to
// stub in unit tests via sha256FactoryFn.

package network

import (
	"crypto/sha256"
	"hash"
)

// sha256HashWrapper adapts hash.Hash to the local sha256Writer interface.
// hash.Hash already satisfies io.Writer and provides Sum(), so this is a
// straightforward type alias rather than a wrapping struct.
type sha256HashWrapper struct {
	hash.Hash
}

// _newSHA256 returns a fresh SHA-256 hash instance wrapped as a sha256Writer.
// It is the default implementation used by newSHA256() in tcp.go when
// sha256FactoryFn has not been overridden by a test.
func _newSHA256() sha256Writer {
	return &sha256HashWrapper{Hash: sha256.New()}
}
