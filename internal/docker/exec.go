package docker

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
)

// runCmd runs an external command, streaming its output to stdout/stderr.
func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %v: %w", name, args, err)
	}
	return nil
}

// killPortHolder kills any process on the host that is listening on the given
// TCP port. This is needed because Appium containers run with --network=host,
// and orphaned Appium processes (left over after a container is force-removed)
// will continue to hold the port, preventing the next container from binding.
func killPortHolder(port int) {
	// ss -Htlnp "sport = :<port>" lists the listener; grep extracts its PID.
	script := fmt.Sprintf(
		`ss -Htlnp 'sport = :%d' | grep -oP 'pid=\K[0-9]+' | xargs -r kill -9`,
		port,
	)
	cmd := exec.Command("sh", "-c", script)
	if out, err := cmd.CombinedOutput(); err == nil {
		if len(out) > 0 {
			log.Printf("[cleanup] killed process holding port %d: %s", port, out)
		}
	}
}
