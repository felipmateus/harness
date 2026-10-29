package daemon

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	// The first signal.Notify of a process starts a goroutine that lives for the rest of the
	// process. Start it before any test takes a goroutine baseline, or the first leak check
	// would count it as a leak. SIGUSR2 is never sent.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGUSR2)
	code := m.Run()
	runCleanups()
	os.Exit(code)
}

// The subprocess suites build a binary and fake a $HOME once for the whole run; they
// register what must go when the last test has finished.
var (
	cleanupMu sync.Mutex
	cleanups  []func()
)

func registerCleanup(f func()) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	cleanups = append(cleanups, f)
}

func runCleanups() {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		cleanups[i]()
	}
	cleanups = nil
}
