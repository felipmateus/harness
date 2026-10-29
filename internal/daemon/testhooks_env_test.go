//go:build harness_testhooks

package daemon

import (
	"strings"
	"testing"
	"time"
)

func TestParseTestHooks_Empty(t *testing.T) {
	for _, spec := range []string{"", "   "} {
		h, err := parseTestHooks(spec)
		if h != nil || err != nil {
			t.Errorf("parseTestHooks(%q) = %v, %v; want nil, nil", spec, h, err)
		}
	}
}

func TestParseTestHooks_BootHooks(t *testing.T) {
	h, err := parseTestHooks("hold-boot-step=10")
	if err != nil || h == nil || h.AfterBootStep == nil {
		t.Fatalf("hold-boot-step: %v, %v", h, err)
	}
	if h.BeforeShutdownStep != nil || h.BlockExit {
		t.Error("hold-boot-step set hooks it did not ask for")
	}
	// A step that is not the held one passes straight through.
	if err := h.AfterBootStep(9); err != nil {
		t.Errorf("step 9 returned %v", err)
	}
	held := make(chan struct{})
	go func() { h.AfterBootStep(10); close(held) }()
	select {
	case <-held:
		t.Fatal("step 10 was not held")
	case <-time.After(50 * time.Millisecond):
	}

	f, err := parseTestHooks("fail-boot-step=10")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.AfterBootStep(9); err != nil {
		t.Errorf("step 9 returned %v", err)
	}
	if err := f.AfterBootStep(10); err == nil {
		t.Error("step 10 did not fail")
	}
}

func TestParseTestHooks_ShutdownAndExit(t *testing.T) {
	h, err := parseTestHooks("hold-shutdown-step=3, block-exit")
	if err != nil || h == nil {
		t.Fatalf("%v, %v", h, err)
	}
	if h.BeforeShutdownStep == nil || !h.BlockExit || h.AfterBootStep != nil {
		t.Errorf("hooks %+v", h)
	}
	h.BeforeShutdownStep(2) // not held
	held := make(chan struct{})
	go func() { h.BeforeShutdownStep(3); close(held) }()
	select {
	case <-held:
		t.Fatal("step 3 was not held")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestParseTestHooks_RejectsWhatItDoesNotKnow(t *testing.T) {
	for _, tc := range []struct{ spec, want string }{
		{"hold-boot", "unknown hook"},
		{"hold-boot-step", "needs a step"},
		{"hold-boot-step=abc", "not a step"},
		{"hold-boot-step=0", "not a step"},
		{"hold-boot-step=13", "not a step"},
		{"fail-boot-step=-1", "not a step"},
		{"hold-shutdown-step=11", "not a step"},
		{"block-exit=1", "takes no value"},
		{"explode", "unknown hook"},
		{"hold-boot-step=10,explode", "unknown hook"},
	} {
		h, err := parseTestHooks(tc.spec)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseTestHooks(%q) = %v, %v; want an error containing %q", tc.spec, h, err, tc.want)
		}
	}
}

// Run takes its hooks from the environment when the caller gives none, and a bad value
// stops the daemon before it boots.
func TestRun_ReadsHooksFromTheEnvironment(t *testing.T) {
	t.Setenv(testHooksEnv, "nonsense")
	d := newDaemon(Options{})
	if d.hooksErr == nil {
		t.Fatal("a bad HARNESS_TESTHOOKS was not reported")
	}
	err := d.run()
	if exit := exitError(t, err); exit.Code != 1 || !strings.Contains(exit.Message, "unknown hook") {
		t.Errorf("exit %+v", exit)
	}

	t.Setenv(testHooksEnv, "block-exit")
	if d := newDaemon(Options{}); !d.hooks.BlockExit {
		t.Error("block-exit was not read from the environment")
	}
	// Hooks given in code win over the environment.
	if d := newDaemon(Options{Hooks: &Hooks{}}); d.hooks.BlockExit {
		t.Error("the environment overrode Options.Hooks")
	}
}
