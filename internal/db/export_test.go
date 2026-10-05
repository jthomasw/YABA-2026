package db

import "time"

// Test-only access to unexported helpers, for the external db_test package.

var (
	ParseSnapshotName = parseSnapshotName
	FirstBackupDelay  = firstBackupDelay
)

// SetBackupTiming shortens BackupLoop's waits and returns a function restoring them.
func SetBackupTiming(start, retry time.Duration) func() {
	oldStart, oldRetry := backupStartDelay, backupRetryAfter
	backupStartDelay, backupRetryAfter = start, retry
	return func() { backupStartDelay, backupRetryAfter = oldStart, oldRetry }
}
