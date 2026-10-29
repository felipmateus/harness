//go:build !harness_testhooks

package daemon

// hooksFromEnv is the production form: the environment never configures hooks. Binaries
// built with -tags harness_testhooks read HARNESS_TESTHOOKS instead (testhooks_env.go).
func hooksFromEnv() (*Hooks, error) { return nil, nil }
