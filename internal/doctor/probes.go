package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/config"
	"github.com/felipmateus/harness/internal/home"
)

// accessWrite is W_OK for access(2); package syscall does not export it.
const accessWrite = 0x2

// The modes the daemon creates its files with (Business Rule 28 and the conventions).
const (
	homeMode   fs.FileMode = 0o700
	socketMode fs.FileMode = 0o600
)

// probeDaemonReachable is the local-mode statement that no daemon answered. The CLI runs the
// local checks only after the daemon failed to answer, so there is nothing to probe: the
// item says so and that the rest ran locally.
func probeDaemonReachable(_ context.Context, s *state) contract.DoctorItem {
	return item(contract.SeverityWarn, "no daemon is listening on %s (checks ran locally)", s.home.SocketPath())
}

// probeHomePermissions reports a home that others can reach. The daemon accepts a home that
// is merely readable by others and never changes it (Business Rule 28): this is where the
// operator is told.
func probeHomePermissions(_ context.Context, s *state) contract.DoctorItem {
	dir := s.home.Dir
	fi, err := os.Lstat(dir)
	switch {
	case missing(err):
		return item(contract.SeverityOK, "%s does not exist yet (the first start creates it, 0700)", dir)
	case err != nil:
		return item(contract.SeverityError, "cannot inspect %s: %v", dir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() || foreignOwner(fi) {
		// home.CheckSecurity names the reason, in the words of the daemon's own refusal.
		var he *home.Error
		if err := s.home.CheckSecurity(); errors.As(err, &he) {
			return withFix(item(contract.SeverityError, "%s", he.Message), "%s", he.Hint)
		}
	}
	if perm := fi.Mode().Perm(); perm != homeMode {
		return withFix(item(contract.SeverityError, "%s is %04o; expected %04o", dir, perm, homeMode),
			"chmod 700 %s", dir)
	}
	return item(contract.SeverityOK, "%s is %04o", dir, homeMode)
}

// foreignOwner reports whether the directory belongs to another user.
func foreignOwner(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) != os.Geteuid()
}

// probeSocketPermissions checks the socket of the daemon that is answering. Only daemon
// mode runs it: with no daemon there is no socket to check.
func probeSocketPermissions(_ context.Context, s *state) contract.DoctorItem {
	sock := s.home.SocketPath()
	fi, err := os.Lstat(sock)
	switch {
	case missing(err):
		return withFix(item(contract.SeverityError, "%s does not exist", sock), "restart the daemon")
	case err != nil:
		return item(contract.SeverityError, "cannot inspect %s: %v", sock, err)
	}
	if perm := fi.Mode().Perm(); perm != socketMode {
		return withFix(item(contract.SeverityError, "%s is %04o; expected %04o", sock, perm, socketMode),
			"chmod 600 %s", sock)
	}
	return item(contract.SeverityOK, "%s is %04o", sock, socketMode)
}

// probeConfigValid checks the file on disk, the one the next start will read. In daemon mode
// it also compares the file with what the daemon loaded.
func probeConfigValid(_ context.Context, s *state) contract.DoctorItem {
	path := s.home.ConfigPath()
	loaded, err := config.Load(path)
	if err != nil {
		var invalid *config.InvalidError
		if errors.As(err, &invalid) && len(invalid.Errors) > 0 {
			msg := invalid.Errors[0].Message
			if more := len(invalid.Errors) - 1; more > 0 {
				msg += fmt.Sprintf(" (and %d more)", more)
			}
			return withFix(item(contract.SeverityError, "%s is invalid: %s; the next start will fail", path, msg),
				"run `harness config validate`")
		}
		return withFix(item(contract.SeverityError, "%s cannot be read: %v; the next start will fail", path, err),
			"check the owner and mode of %s", path)
	}

	if s.mode == ModeDaemon && !sameDigest(loaded.Digest, s.loadedDigest) {
		return withFix(item(contract.SeverityWarn, "%s changed since the daemon loaded it; restart to apply", path),
			"run `harness daemon stop` and then `harness daemon start`")
	}
	if !loaded.Exists {
		return item(contract.SeverityOK, "defaults in use (no config file)")
	}
	return item(contract.SeverityOK, "%s is valid", path)
}

func sameDigest(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// probeLedgerIntegrity runs quick_check on a read-only connection. A home that never
// started has no ledger yet, which is healthy.
func probeLedgerIntegrity(ctx context.Context, s *state) contract.DoctorItem {
	path := s.home.DBPath()
	in, err := s.inspectLedger(ctx)
	switch {
	case missing(err):
		return item(contract.SeverityOK, "no ledger yet (%s is created on the first start)", path)
	case errors.Is(err, fs.ErrPermission):
		return withFix(item(contract.SeverityError, "cannot read %s: permission denied", path), "chmod 600 %s", path)
	case err != nil:
		return withFix(item(contract.SeverityError, "cannot check %s: %v", path, err),
			"restore %s from a backup", filepath.Base(path))
	case !in.IntegrityOK():
		return withFix(item(contract.SeverityError, "quick_check found problems: %s", summarize(in.QuickCheck)),
			"restore %s from a backup", filepath.Base(path))
	}
	return item(contract.SeverityOK, "quick_check ok")
}

// summarize joins the first rows of a quick_check answer.
func summarize(rows []string) string {
	const keep = 3
	if len(rows) > keep {
		return strings.Join(rows[:keep], "; ") + fmt.Sprintf("; and %d more", len(rows)-keep)
	}
	return strings.Join(rows, "; ")
}

// probeLedgerSchema compares the schema version in the file with the one this binary embeds.
func probeLedgerSchema(ctx context.Context, s *state) contract.DoctorItem {
	in, err := s.inspectLedger(ctx)
	switch {
	case missing(err):
		return item(contract.SeverityOK, "no ledger yet (the first start creates it)")
	case err != nil:
		// The root cause is already the error of ledger.integrity; do not count it twice.
		return item(contract.SeverityWarn, "schema version unknown: the ledger could not be read (see %s)", IDLedgerIntegrity)
	case in.SchemaAhead():
		return withFix(item(contract.SeverityError, "ledger schema %d is newer than this binary supports (%d)",
			in.SchemaVersion, in.BinaryVersion), "upgrade harness")
	case in.SchemaVersion < in.BinaryVersion:
		return item(contract.SeverityWarn, "schema %d is older than this binary's %d (the next start migrates it)",
			in.SchemaVersion, in.BinaryVersion)
	}
	return item(contract.SeverityOK, "schema %d (current)", in.SchemaVersion)
}

// probeLogWritable checks that the log file can be written, without writing: it opens an
// existing file for writing and writes nothing, which changes neither content nor mtime.
func probeLogWritable(_ context.Context, s *state) contract.DoctorItem {
	path := s.home.LogPath()
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil {
		f.Close()
		return item(contract.SeverityOK, "%s is writable", path)
	}
	if !missing(err) {
		return withFix(item(contract.SeverityError, "%s is not writable: %v", path, err), "chmod 600 %s", path)
	}

	// No log yet: the daemon will create it, so what matters is the directory it goes in.
	dir := s.home.LogsDir()
	if _, err := os.Stat(dir); missing(err) {
		dir = s.home.Dir
		if _, err := os.Stat(dir); missing(err) {
			return item(contract.SeverityOK, "%s does not exist yet (the first start creates it)", path)
		}
	}
	if err := syscall.Access(dir, accessWrite); err != nil {
		return withFix(item(contract.SeverityError, "%s does not exist and %s is not writable: %v", path, dir, err),
			"chmod 700 %s", dir)
	}
	return item(contract.SeverityOK, "%s does not exist yet (the first start creates it)", path)
}
