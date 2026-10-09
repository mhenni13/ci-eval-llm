package runtime

import (
	"os"
	"syscall"
)

// processAlive reports whether a PID is a live process (signal 0 probe).
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

func syscallTerm() os.Signal { return syscall.SIGTERM }
