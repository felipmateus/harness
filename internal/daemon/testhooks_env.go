//go:build harness_testhooks

package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// testHooksEnv names the variable that configures the hooks of a hook-enabled binary.
const testHooksEnv = "HARNESS_TESTHOOKS"

// hooksFromEnv builds the hooks HARNESS_TESTHOOKS asks for, or nil when it is unset or
// empty. This file is compiled only with -tags harness_testhooks, so a release binary has
// no way to hold, fail or block a daemon from its environment.
func hooksFromEnv() (*Hooks, error) {
	return parseTestHooks(os.Getenv(testHooksEnv))
}

// parseTestHooks reads a comma-separated list of hooks:
//
//	hold-boot-step=N      block after boot step N, until the boot is cancelled
//	fail-boot-step=N      fail boot step N
//	hold-shutdown-step=N  block before shutdown step N, until the budget or a second signal
//	block-exit            never complete the shutdown: keep the home locked, do not exit
//
// An unknown hook, a missing or malformed step, or a step outside 1..12 (boot) or 1..10
// (shutdown) is an error: a test that mistypes a hook must fail, not run without it.
func parseTestHooks(spec string) (*Hooks, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var (
		holdBoot, failBoot, holdShutdown int
		blockExit                        bool
	)
	for _, item := range strings.Split(spec, ",") {
		name, value, hasValue := strings.Cut(strings.TrimSpace(item), "=")
		switch name {
		case "hold-boot-step", "fail-boot-step":
			n, err := stepArg(name, value, hasValue, 12)
			if err != nil {
				return nil, err
			}
			if name == "hold-boot-step" {
				holdBoot = n
			} else {
				failBoot = n
			}
		case "hold-shutdown-step":
			n, err := stepArg(name, value, hasValue, 10)
			if err != nil {
				return nil, err
			}
			holdShutdown = n
		case "block-exit":
			if hasValue {
				return nil, fmt.Errorf("%s: block-exit takes no value", testHooksEnv)
			}
			blockExit = true
		default:
			return nil, fmt.Errorf("%s: unknown hook %q", testHooksEnv, name)
		}
	}
	h := &Hooks{BlockExit: blockExit}
	if holdBoot != 0 || failBoot != 0 {
		h.AfterBootStep = func(step int) error {
			if step == failBoot {
				return errors.New("test hook: fail-boot-step")
			}
			if step == holdBoot {
				blockForever()
			}
			return nil
		}
	}
	if holdShutdown != 0 {
		h.BeforeShutdownStep = func(step int) {
			if step == holdShutdown {
				blockForever()
			}
		}
	}
	return h, nil
}

func stepArg(name, value string, hasValue bool, max int) (int, error) {
	if !hasValue {
		return 0, fmt.Errorf("%s: %s needs a step, as %s=N", testHooksEnv, name, name)
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > max {
		return 0, fmt.Errorf("%s: %s=%q is not a step from 1 to %d", testHooksEnv, name, value, max)
	}
	return n, nil
}
