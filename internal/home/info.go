package home

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Info is the content of <home>/daemon.json. It is for discovery and diagnostics only;
// ownership is decided by the lock.
type Info struct {
	PID       int    `json:"pid"`
	Version   string `json:"version"`
	Socket    string `json:"socket"`
	StartedAt string `json:"started_at"`
}

// renameFile is a seam for the crash-between-write-and-rename test.
var renameFile = os.Rename

// WriteInfo replaces daemon.json atomically: temp file in the home, fsync, rename,
// directory fsync. Readers see the old file, the new file, or none; never a partial one.
func (h Home) WriteInfo(info Info) (err error) {
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("encode daemon.json: %w", err)
	}
	tmp, err := os.CreateTemp(h.Dir, ".daemon.json.*") // created 0600
	if err != nil {
		return fmt.Errorf("create temp daemon.json: %w", err)
	}
	defer func() {
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp daemon.json: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp daemon.json: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp daemon.json: %w", err)
	}
	if err = renameFile(tmp.Name(), h.InfoPath()); err != nil {
		return fmt.Errorf("install daemon.json: %w", err)
	}
	return syncDir(h.Dir)
}

// ReadInfo reads daemon.json.
func (h Home) ReadInfo() (Info, error) {
	data, err := os.ReadFile(h.InfoPath())
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, fmt.Errorf("decode %s: %w", h.InfoPath(), err)
	}
	return info, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open %s for sync: %w", dir, err)
	}
	return errors.Join(d.Sync(), d.Close())
}
