// Package version reports the build identity of the harness binary.
package version

import "runtime"

// Version and Commit are set at build time with -ldflags "-X".
var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)

// Info is the build identity shown by `harness version` and recorded by the daemon.
type Info struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"`
	Go       string `json:"go"`
	Platform string `json:"platform"`
}

// Get returns the identity of the running binary.
func Get() Info {
	return Info{
		Version:  Version,
		Commit:   Commit,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
}
