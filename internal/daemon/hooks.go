package daemon

import (
	"log/slog"
	"time"

	"github.com/felipmateus/harness/internal/admission"
	"github.com/felipmateus/harness/internal/ledger"
)

// Hooks lets a test hold or fail boot and shutdown steps and reach the daemon's internals.
// It is nil in production. In-process tests set Options.Hooks directly; the subprocess form
// (HARNESS_TESTHOOKS, compiled only with the harness_testhooks build tag) maps onto this
// struct.
type Hooks struct {
	// AfterBootStep runs after boot step 1..12 has completed. Returning an error makes
	// that step fail, which simulates a failure at that point of the boot.
	//
	// It may block: the daemon waits for it in a goroutine of its own, so a boot that is
	// cancelled (signal, shutdown request or context) while the hook is held is not held
	// with it. A hook that is abandoned that way keeps running until it returns, and the
	// daemon ignores its result. A cancelled boot does not start the hook at all.
	AfterBootStep func(step int) error

	// BeforeShutdownStep runs before shutdown step 1..10. It may block, for example to hold
	// the drain at step 3. The step runs inside the shutdown budget, so a held hook is
	// ended by the budget or by a second signal; once the daemon has given up on the step,
	// the step's action never runs when the hook returns.
	BeforeShutdownStep func(step int)

	// OnRunning is called once, right after the daemon reports running (gate running, log
	// record written). It runs on the boot goroutine: hand the access to the test and
	// return.
	OnRunning func(TestAccess)

	// BlockExit makes the shutdown never complete: the daemon keeps the home's lock (the
	// forced path included) and, once the shutdown sequence has ended, blocks instead of
	// returning, so only a kill ends the process. It is for the subprocess tests that
	// check what `harness daemon stop` does when a daemon does not release the home.
	BlockExit bool

	// KeepaliveInterval overrides the 15 s SSE keepalive; zero keeps the default.
	KeepaliveInterval time.Duration
	// WriteTimeout overrides the 10 s write deadline of the SSE adapter; zero keeps the
	// default.
	WriteTimeout time.Duration
}

// TestAccess is what OnRunning hands to a test: the real components of the running daemon.
type TestAccess struct {
	// Appender writes to the ledger exactly as the daemon does: durable before visible.
	Appender ledger.Appender
	// Gate is the admission gate. A test that calls Enter holds an in-flight request,
	// which the shutdown drain (step 3) then waits for.
	Gate admission.Gate
	// Logger is the daemon's logger: redacting, written to harness.log.
	Logger *slog.Logger
}
