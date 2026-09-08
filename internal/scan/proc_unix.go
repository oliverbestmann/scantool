//go:build unix

package scan

import (
	"os/exec"
	"syscall"
	"time"
)

// killGrace is how long a cancelled scan may take to shut down before it is
// killed outright.
const killGrace = 3 * time.Second

// setupProcessGroup puts the scan command into its own process group and
// cancels that whole group. Without this, aborting a scan would only kill the
// script: the scanimage it started would keep running, keep the scanner busy
// and keep the output pipe open, so the scan would appear to hang until it
// finished on its own.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}

		// A negative pid addresses the process group.
		pgid := -cmd.Process.Pid
		err := syscall.Kill(pgid, syscall.SIGTERM)

		// Give scanimage a moment to release the device, then insist.
		time.AfterFunc(killGrace, func() { syscall.Kill(pgid, syscall.SIGKILL) })
		return err
	}

	// Stop waiting for output from anything that survived the signals.
	cmd.WaitDelay = killGrace + time.Second
}
