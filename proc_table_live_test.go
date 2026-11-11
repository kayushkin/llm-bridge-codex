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
	// The shell opens the file before it execs sleep, so the file is held
	// by the shell for a moment first: wait for both, the file held and the
	// process turned into sleep.
	table := procProcessTable{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		holders, err := table.holdersOf(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(holders) == 1 && holders[0] == holder.Process.Pid {
			process, err := table.process(holder.Process.Pid)
			if err != nil {
				t.Fatal(err)
			}
			if len(process.arguments) > 0 && filepath.Base(process.arguments[0]) == "sleep" {
				if process.parentPID != os.Getpid() {
					t.Fatalf("process = %+v, want sleep under pid %d", process, os.Getpid())
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("holdersOf = %v, want [%d] running sleep", holders, holder.Process.Pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The /proc reader reads a real process's session id from its environment,
// and tells a variable that is unset from one that is empty.
func TestProcProcessTableReadsAProcessEnvironmentVariable(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("no /proc")
	}
	child := exec.Command("sleep", "30")
	child.Env = []string{bridgeSessionIDEnvironmentVariable + "=br_live", "EMPTY_VARIABLE="}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	// Until the child execs sleep, /proc shows the test's own environment.
	table := procProcessTable{}
	var value string
	var set bool
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		value, set, err = table.environmentValue(child.Process.Pid, bridgeSessionIDEnvironmentVariable)
		if err != nil || set {
			break
		}
	}
	if err != nil || !set || value != "br_live" {
		t.Fatalf("environmentValue = %q, %v, %v; want br_live, set", value, set, err)
	}
	if value, set, err := table.environmentValue(child.Process.Pid, "EMPTY_VARIABLE"); err != nil || !set || value != "" {
		t.Fatalf("EMPTY_VARIABLE = %q, %v, %v; want empty and set", value, set, err)
	}
	if _, set, err := table.environmentValue(child.Process.Pid, "NOT_SET_ANYWHERE"); err != nil || set {
		t.Fatalf("NOT_SET_ANYWHERE: set=%v err=%v; want unset", set, err)
	}
}
