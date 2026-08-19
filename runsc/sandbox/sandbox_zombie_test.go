package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestIsRunningZombieNotRunning verifies that IsRunning treats an exited but
// unreaped (zombie) sandbox process as not running. Signal 0 succeeds on
// zombies, so a liveness check based on signal 0 alone reports a detached
// sandbox whose init died as "running" forever.
func TestIsRunningZombieNotRunning(t *testing.T) {
	// Spawn a child that exits immediately and never reap it, so it stays a
	// zombie for the lifetime of the test process.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start child: %v", err)
	}
	defer func() {
		// Reap at the end to avoid leaking the zombie.
		_ = cmd.Wait()
	}()
	// Wait for the child to become a zombie (i.e. to have exited without
	// being reaped).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			t.Fatalf("child was reaped unexpectedly")
		}
		// Poll /proc for the zombie state.
		if procStateIsZombie(t, cmd.Process.Pid) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !procStateIsZombie(t, cmd.Process.Pid) {
		t.Fatalf("child did not become a zombie")
	}

	s := &Sandbox{}
	s.Pid.Store(cmd.Process.Pid)
	if s.IsRunning() {
		t.Errorf("IsRunning() = true for zombie sandbox process %d, want false", cmd.Process.Pid)
	}
	// A live process must still be reported as running (self).
	live := &Sandbox{}
	live.Pid.Store(os.Getpid())
	if !live.IsRunning() {
		t.Errorf("IsRunning() = false for live process %d, want true", os.Getpid())
	}
	// A nonexistent process must not be reported as running.
	gone := &Sandbox{}
	gone.Pid.Store(1 << 30)
	if gone.IsRunning() {
		t.Errorf("IsRunning() = true for nonexistent pid, want false")
	}
}

// procStateIsZombie reports whether /proc/<pid>/stat shows state Z.
func procStateIsZombie(t *testing.T, pid int) bool {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	if i := bytes.LastIndexByte(b, ')'); i >= 0 && i+2 < len(b) {
		return b[i+2] == 'Z'
	}
	return false
}
