//go:build !unix

package mcp

import "github.com/vanclief/ez"

// mapGuestMemory has no mapping on this platform, so wazero grows the
// guest's memory on the Go heap, copying it as it grows.
func mapGuestMemory(size int) ([]byte, error) {
	return nil, ez.New(ez.EUNAVAILABLE, "mcp: no memory mapping on this platform", nil)
}

func unmapGuestMemory(buf []byte) {}
