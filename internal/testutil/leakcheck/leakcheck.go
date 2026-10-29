// Package leakcheck detects goroutines that outlive a test, without extra dependencies.
// It compares runtime.NumGoroutine against a baseline taken when the check is armed.
package leakcheck

import (
	"runtime"
	"testing"
	"time"
)

// Baseline returns the number of goroutines now. Take it before starting the goroutines
// under test, and before opening anything else that owns one.
func Baseline() int { return runtime.NumGoroutine() }

// Settle polls until the goroutine count is at most baseline or d elapses, and returns the
// last count. A goroutine that has been told to stop needs a moment to be scheduled and
// return, so a single reading would be flaky.
func Settle(baseline int, d time.Duration) int {
	deadline := time.Now().Add(d)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline || !time.Now().Before(deadline) {
			return n
		}
		time.Sleep(time.Millisecond)
	}
}

// Check arms a cleanup that fails the test if goroutines started after this call are still
// running up to three seconds after the test ended. Call it first in the test: cleanups
// run last-in first-out, so resources registered later (a ledger, a broker) are closed
// before the count is taken.
func Check(t testing.TB) {
	t.Helper()
	baseline := Baseline()
	t.Cleanup(func() {
		if n := Settle(baseline, 3*time.Second); n > baseline {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Errorf("goroutine leak: %d running, baseline %d\n%s", n, baseline, buf)
		}
	})
}
