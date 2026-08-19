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

// TestStatIsZombieParsing pins the /proc/[pid]/stat parse against comm
// values containing ')' and spaces, which the field itself permits.
func TestStatIsZombieParsing(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want bool
	}{
		{"plain running", "17 (runsc-sandbox) R 1 ...", false},
		{"plain zombie", "17 (runsc-sandbox) Z 1 ...", true},
		{"comm with parens and spaces, zombie", `17 (a ) b) Z 1 ...`, true},
		{"comm with parens and spaces, running", `17 (a ) b) S 1 ...`, false},
		{"comm of parens running", `17 ()))))))))))))) R 1 ...`, false},
		{"comm with state-looking suffix", `17 (a) Z (running) S 1 ...`, false},
		{"comm containing Z", `17 (x) Z (running) Z 1 ...`, true},
		{"empty comm", `17 () X 1 ...`, false},
		{"truncated line", `17 (runsc`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := statIsZombie([]byte(tc.line)); got != tc.want {
				t.Errorf("statIsZombie(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
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
