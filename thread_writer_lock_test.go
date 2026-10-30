package main

import (
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeProcessTable stands in for /proc. A process dies on the signal named in
// diesOn (SIGTERM unless set), and the lock is free once its holders are dead.
type fakeProcessTable struct {
	processes map[int]codexProcess
	holders   []int
	diesOn    map[int]syscall.Signal
	dead      map[int]bool
	signals   []string
}

func (table *fakeProcessTable) holdersOf(string) ([]int, error) {
	var alive []int
	for _, pid := range table.holders {
		if !table.dead[pid] {
			alive = append(alive, pid)
		}
	}
	return alive, nil
}

func (table *fakeProcessTable) process(pid int) (codexProcess, error) {
	process, found := table.processes[pid]
	if !found {
		return codexProcess{}, fmt.Errorf("no process %d", pid)
	}
	return process, nil
}

func (table *fakeProcessTable) signal(pid int, signal syscall.Signal) error {
	table.signals = append(table.signals, fmt.Sprintf("%d:%d", pid, signal))
	want := syscall.SIGTERM
	if dieOn, found := table.diesOn[pid]; found {
		want = dieOn
	}
	if signal == want || signal == syscall.SIGKILL {
		table.dead[pid] = true
	}
	return nil
}

// The tree measured on a laptop on 2026-10-30: a codex terminal (133) under a
// shell, codex's daemon supervisor (229) under it, and the shared background
// app-server (237812) holding the lock; plus this wrapper (340992) and its own
// app-server (341002) under llm-bridge-server.
func laptopProcessTree() *fakeProcessTable {
	daemon := "/home/user/.codex/packages/app-server-daemon/releases/0.166.0/bin/codex"
	return &fakeProcessTable{
		processes: map[int]codexProcess{
			11:     {pid: 11, parentPID: 10, arguments: []string{"bash"}},
			10:     {pid: 10, parentPID: 1, arguments: []string{"Relay"}},
			133:    {pid: 133, parentPID: 11, arguments: []string{"codex", "resume", "--all"}},
			229:    {pid: 229, parentPID: 133, arguments: []string{daemon, "app-server", "daemon", "pid-update-loop"}},
			237812: {pid: 237812, parentPID: 229, arguments: []string{daemon, "app-server", "--listen", "unix://", "--managed-daemon"}},
			261431: {pid: 261431, parentPID: 1, arguments: []string{"/usr/local/bin/llm-bridge-server"}},
			340992: {pid: 340992, parentPID: 261431, arguments: []string{"/home/user/bin/llm-bridge-codex"}},
			341002: {pid: 341002, parentPID: 340992, arguments: []string{"codex", "app-server", "--listen", "ws://127.0.0.1:44551"}},
		},
		holders: []int{237812},
		diesOn:  map[int]syscall.Signal{},
		dead:    map[int]bool{},
	}
}

func stoppedPIDs(stopped []codexProcess) []int {
	pids := make([]int, len(stopped))
	for index, process := range stopped {
		pids[index] = process.pid
	}
	return pids
}

// The terminal, its supervisor and the background server all stop, terminal
// first, and the shell above them does not.
func TestReleaseThreadWriterLockClosesTheCodexTerminalAboveTheBackgroundServer(t *testing.T) {
	table := laptopProcessTree()
	stopped, err := releaseThreadWriterLock(table, "/lock", 340992, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(stoppedPIDs(stopped)); got != "[133 229 237812]" {
		t.Fatalf("stopped %s, want [133 229 237812]", got)
	}
	if got := strings.Join(table.signals, " "); got != "133:15 229:15 237812:15" {
		t.Fatalf("signals %q, want SIGTERM to each, terminal first", got)
	}
	if table.dead[11] {
		t.Fatal("stopped the shell above the codex terminal")
	}
}

// A codex that ignores SIGTERM gets SIGKILL once the wait runs out.
func TestReleaseThreadWriterLockKillsAHolderThatIgnoresSIGTERM(t *testing.T) {
	table := laptopProcessTree()
	table.diesOn[237812] = syscall.SIGKILL
	if _, err := releaseThreadWriterLock(table, "/lock", 340992, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(table.signals, " "), "237812:9") {
		t.Fatalf("signals %v: no SIGKILL to the holder", table.signals)
	}
}

// Another bridge session's codex holds the thread: stop nothing.
func TestReleaseThreadWriterLockLeavesAnotherBridgeSessionsCodexAlone(t *testing.T) {
	table := laptopProcessTree()
	table.holders = []int{341002}
	_, err := releaseThreadWriterLock(table, "/lock", 999, time.Second)
	if err == nil || !strings.Contains(err.Error(), "another bridge session") {
		t.Fatalf("err = %v, want a refusal naming another bridge session", err)
	}
	if len(table.signals) != 0 {
		t.Fatalf("sent %v", table.signals)
	}
}

// Codex refused, yet nothing holds the lock: say so rather than guess.
func TestReleaseThreadWriterLockFailsWhenNoProcessHoldsTheLock(t *testing.T) {
	table := laptopProcessTree()
	table.holders = nil
	if _, err := releaseThreadWriterLock(table, "/lock", 340992, time.Second); err == nil {
		t.Fatal("want an error when no process holds the lock")
	}
}

// Something other than codex holds the lock file: stop nothing.
func TestReleaseThreadWriterLockLeavesAHolderThatIsNotCodexAlone(t *testing.T) {
	table := laptopProcessTree()
	table.holders = []int{11}
	if _, err := releaseThreadWriterLock(table, "/lock", 340992, time.Second); err == nil || len(table.signals) != 0 {
		t.Fatalf("err = %v, signals %v; want a refusal and no signal", err, table.signals)
	}
}
