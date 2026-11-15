package relational

import (
	"testing"
	"time"
)

// CreateLockKey - createLockKey, for tests that hold the create lock themselves
const CreateLockKey = createLockKey

// SetCreateLockWait - Sets createLockWait for the test t and restores it when t ends
func SetCreateLockWait(t *testing.T, wait time.Duration) {
	previous := createLockWait
	createLockWait = wait
	t.Cleanup(func() {
		createLockWait = previous
	})
}

// SetAdvisoryUnlockTimeout - Sets advisoryUnlockTimeout for the test t and
// restores it when t ends
func SetAdvisoryUnlockTimeout(t *testing.T, timeout time.Duration) {
	previous := advisoryUnlockTimeout
	advisoryUnlockTimeout = timeout
	t.Cleanup(func() {
		advisoryUnlockTimeout = previous
	})
}
