//go:build race

package tools

// raceEnabled is true when the binary is built with -race (ThreadSanitizer).
// Under TSan the runtime maps a large shadow-memory region, so setting
// RLIMIT_AS/RLIMIT_DATA on the current process can cause OOM. Resource-limit
// enforcement is skipped in that case.
const raceEnabled = true
