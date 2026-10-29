// Package config loads and validates <home>/config.toml. Decoding is strict: unknown
// keys, wrong types and out-of-range values make the file invalid, and every problem is
// collected so `harness config validate` can report them all at once.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

// Error codes for FieldError.
const (
	CodeParseError   = "parse_error"
	CodeUnknownKey   = "unknown_key"
	CodeInvalidValue = "invalid_value"
	CodeOutOfRange   = "out_of_range"
)

const (
	minShutdownGrace = time.Second
	maxShutdownGrace = 5 * time.Minute
)

var logLevels = []string{"debug", "info", "warn", "error"}

// Duration keeps the text the operator wrote so `config show` prints it back unchanged.
type Duration struct {
	time.Duration
	raw string
	// set is true once the file has given the value, whatever its text. It is separate from
	// raw because an empty string is a written value, and an invalid one, not an absent one.
	set bool
}

func (d *Duration) UnmarshalText(text []byte) error {
	d.raw = string(text) // parsed and range-checked in validate, so errors are collected
	d.set = true
	return nil
}

func (d Duration) MarshalText() ([]byte, error) {
	if d.raw != "" {
		return []byte(d.raw), nil
	}
	return []byte(d.Duration.String()), nil
}

// Config is the effective configuration.
type Config struct {
	Daemon DaemonConfig `toml:"daemon" json:"daemon"`
	Log    LogConfig    `toml:"log" json:"log"`
}

type DaemonConfig struct {
	ShutdownGrace Duration `toml:"shutdown_grace" json:"shutdown_grace"`
}

type LogConfig struct {
	Level string `toml:"level" json:"level"`
}

// Default returns the built-in defaults.
func Default() Config {
	return Config{
		Daemon: DaemonConfig{ShutdownGrace: Duration{Duration: 30 * time.Second, raw: "30s"}},
		Log:    LogConfig{Level: "info"},
	}
}

// Loaded is the result of reading a config file.
type Loaded struct {
	Config Config
	Path   string
	Exists bool
	Digest *string // "sha256:<hex>" of the file bytes; nil when the file does not exist
}

// FieldError is one validation problem.
type FieldError struct {
	Key     string `json:"key"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// InvalidError lists every problem found in a config file.
type InvalidError struct {
	Path   string
	Errors []FieldError
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Errors[0].Message)
}

// Load reads path. A missing file yields the defaults; an invalid file yields
// *InvalidError with every problem found.
func Load(path string) (Loaded, error) {
	loaded := Loaded{Config: Default(), Path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return loaded, nil
	}
	if err != nil {
		return Loaded{}, fmt.Errorf("read config %s: %w", path, err)
	}
	loaded.Exists = true
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	loaded.Digest = &digest

	cfg := Default()
	md, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Loaded{}, &InvalidError{Path: path, Errors: []FieldError{{Code: CodeParseError, Message: err.Error()}}}
	}
	var problems []FieldError
	for _, key := range md.Undecoded() {
		problems = append(problems, FieldError{
			Key:     key.String(),
			Code:    CodeUnknownKey,
			Message: fmt.Sprintf("unknown key %q", key.String()),
		})
	}
	problems = append(problems, validate(&cfg)...)
	if len(problems) > 0 {
		return Loaded{}, &InvalidError{Path: path, Errors: problems}
	}
	loaded.Config = cfg
	return loaded, nil
}

func validate(cfg *Config) []FieldError {
	var problems []FieldError
	grace := &cfg.Daemon.ShutdownGrace
	if grace.set {
		d, err := time.ParseDuration(grace.raw)
		switch {
		case err != nil:
			problems = append(problems, FieldError{
				Key:     "daemon.shutdown_grace",
				Code:    CodeInvalidValue,
				Message: fmt.Sprintf("daemon.shutdown_grace %q is not a duration", grace.raw),
			})
		case d < minShutdownGrace || d > maxShutdownGrace:
			problems = append(problems, FieldError{
				Key:     "daemon.shutdown_grace",
				Code:    CodeOutOfRange,
				Message: fmt.Sprintf("daemon.shutdown_grace %q must be between 1s and 5m", grace.raw),
			})
		default:
			grace.Duration = d
		}
	}
	if !validLevel(cfg.Log.Level) {
		problems = append(problems, FieldError{
			Key:     "log.level",
			Code:    CodeInvalidValue,
			Message: fmt.Sprintf("log.level %q must be one of debug, info, warn, error", cfg.Log.Level),
		})
	}
	return problems
}

func validLevel(level string) bool {
	for _, l := range logLevels {
		if l == level {
			return true
		}
	}
	return false
}
