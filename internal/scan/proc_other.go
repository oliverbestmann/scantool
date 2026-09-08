//go:build !unix

package scan

import (
	"os/exec"
	"time"
)

// setupProcessGroup is a no-op outside unix; os/exec then kills just the
// command itself when a scan is cancelled.
func setupProcessGroup(cmd *exec.Cmd) {
	cmd.WaitDelay = 4 * time.Second
}
