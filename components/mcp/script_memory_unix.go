//go:build unix

package mcp

import (
	"syscall"

	"github.com/vanclief/ez"
)

// mapGuestMemory reserves size bytes of anonymous private memory. The
// kernel commits pages as they are touched; unmapGuestMemory returns them.
func mapGuestMemory(size int) ([]byte, error) {
	buf, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, ez.New(ez.ERESOURCEEXHAUSTED, "mcp: cannot map memory for the script; try again shortly", err)
	}
	return buf, nil
}

func unmapGuestMemory(buf []byte) {
	_ = syscall.Munmap(buf)
}
