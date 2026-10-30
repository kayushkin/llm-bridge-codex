package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The /proc reader finds a real process holding a file open, and its parent.
func TestProcProcessTableFindsTheProcessHoldingAFile(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc")
	}
	path := filepath.Join(t.TempDir(), "thread.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	holder := exec.Command("sh", "-c", `exec 3<"$0"; exec sleep 30`, path)
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	table := procProcessTable{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		holders, err := table.holdersOf(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(holders) == 1 && holders[0] == holder.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("holdersOf = %v, want [%d]", holders, holder.Process.Pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	process, err := table.process(holder.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if process.parentPID != os.Getpid() || filepath.Base(process.arguments[0]) != "sleep" {
		t.Fatalf("process = %+v, want sleep under pid %d", process, os.Getpid())
	}
}
