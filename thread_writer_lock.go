package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// codexActiveWriterRefusal is the text of codex's refusal to resume a thread
// that another codex process is writing: "thread/resume: rpc error -32600:
// thread … already has an active writer". Codex gives the refusal no code of
// its own (-32600 is any invalid request), so the text is all there is.
const codexActiveWriterRefusal = "already has an active writer"

// threadWriterLockPath is the file codex holds open while a process writes the
// thread. Codex removes it when the writer exits.
func threadWriterLockPath(codexHome, threadID string) string {
	return filepath.Join(codexHome, "thread-writer-locks", threadID+".lock")
}

// codexProcess is one process as the lock release sees it.
type codexProcess struct {
	pid       int
	parentPID int
	arguments []string
}

func (p codexProcess) String() string {
	return fmt.Sprintf("pid %d (%s)", p.pid, strings.Join(p.arguments, " "))
}

// isCodex reports whether the process runs the codex CLI: its own binary
// (`codex …`, or a release path ending in /codex), or the npm launcher that
// runs it under node.
func (p codexProcess) isCodex() bool {
	for index, argument := range p.arguments {
		if index > 1 {
			break
		}
		if filepath.Base(argument) == "codex" || filepath.Base(argument) == "codex.js" {
			return true
		}
	}
	return false
}

// isBridgeWrapper reports whether the process is an llm-bridge-codex wrapper,
// which means the codex under it belongs to a bridge session.
func (p codexProcess) isBridgeWrapper() bool {
	return len(p.arguments) > 0 && filepath.Base(p.arguments[0]) == "llm-bridge-codex"
}

// processTable is the part of the operating system the lock release uses, so
// tests can stand in for /proc and for signals.
type processTable interface {
	holdersOf(path string) ([]int, error)
	process(pid int) (codexProcess, error)
	signal(pid int, signal syscall.Signal) error
}

// releaseThreadWriterLock stops the codex terminal that holds a thread's
// writer lock, so this session's app-server can resume the thread.
//
// The holder is usually codex's shared background app-server, which a codex
// terminal (`codex`, `codex resume`) starts and hands its threads to. Stopping
// only that server would leave the terminal running, and the terminal would
// start a new one and take the thread back, so the release also stops every
// codex process above the holder: the server's supervisor and the terminal
// itself. It stops nothing that runs under an llm-bridge-codex wrapper, since
// that codex belongs to another bridge session, and nothing in this process's
// own tree; either one is an error naming the holder.
//
// It sends SIGTERM top-down, waits for the lock to be free, then sends SIGKILL
// to what is left. It returns the processes it stopped.
func releaseThreadWriterLock(table processTable, lockPath string, ownPID int, wait time.Duration) ([]codexProcess, error) {
	holders, err := table.holdersOf(lockPath)
	if err != nil {
		return nil, fmt.Errorf("find the process holding %s: %w", lockPath, err)
	}
	if len(holders) == 0 {
		return nil, fmt.Errorf("codex says another process is writing the thread, but no process holds %s open", lockPath)
	}

	var stopping []codexProcess
	seen := map[int]bool{}
	for _, holderPID := range holders {
		holder, err := table.process(holderPID)
		if err != nil {
			return nil, fmt.Errorf("read the process holding %s: %w", lockPath, err)
		}
		if !holder.isCodex() {
			return nil, fmt.Errorf("%s holds %s and is not codex; not stopping it", holder, lockPath)
		}
		chain := []codexProcess{holder}
		for current := holder; ; {
			if current.pid == ownPID {
				return nil, fmt.Errorf("%s holds %s and runs under this wrapper; not stopping it", holder, lockPath)
			}
			if current.isBridgeWrapper() {
				return nil, fmt.Errorf("%s holds %s and belongs to another bridge session (wrapper pid %d); not stopping it", holder, lockPath, current.pid)
			}
			if current.parentPID <= 1 {
				break
			}
			parent, err := table.process(current.parentPID)
			if err != nil {
				break
			}
			if parent.isCodex() {
				chain = append(chain, parent)
			}
			current = parent
		}
		// The walk goes on past the codex processes, so a bridge wrapper
		// anywhere above the holder still refuses; but the release stops only
		// the unbroken run of codex processes from the holder upward.
		var run []codexProcess
		for index, process := range chain {
			if index > 0 && chain[index-1].parentPID != process.pid {
				break
			}
			run = append(run, process)
		}
		for index := len(run) - 1; index >= 0; index-- {
			if !seen[run[index].pid] {
				seen[run[index].pid] = true
				stopping = append(stopping, run[index])
			}
		}
	}

	for _, process := range stopping {
		if err := table.signal(process.pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
			return nil, fmt.Errorf("stop %s: %w", process, err)
		}
	}
	if lockFreed(table, lockPath, wait) {
		return stopping, nil
	}
	for _, process := range stopping {
		if err := table.signal(process.pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return nil, fmt.Errorf("kill %s: %w", process, err)
		}
	}
	if lockFreed(table, lockPath, wait) {
		return stopping, nil
	}
	return nil, fmt.Errorf("stopped %d codex processes, but %s is still held", len(stopping), lockPath)
}

// lockFreed waits up to wait for no process to hold the lock.
func lockFreed(table processTable, lockPath string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		holders, err := table.holdersOf(lockPath)
		if err == nil && len(holders) == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// procProcessTable reads /proc. It works on Linux, WSL included; elsewhere
// holdersOf fails and the resume fails with codex's own refusal.
type procProcessTable struct{}

func (procProcessTable) holdersOf(path string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var holders []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		descriptorDirectory := filepath.Join("/proc", entry.Name(), "fd")
		descriptors, err := os.ReadDir(descriptorDirectory)
		if err != nil {
			continue // another user's process, or one that just exited
		}
		for _, descriptor := range descriptors {
			target, err := os.Readlink(filepath.Join(descriptorDirectory, descriptor.Name()))
			if err == nil && target == path {
				holders = append(holders, pid)
				break
			}
		}
	}
	return holders, nil
}

func (procProcessTable) process(pid int) (codexProcess, error) {
	commandLine, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return codexProcess{}, err
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return codexProcess{}, err
	}
	parentPID := -1
	for _, line := range strings.Split(string(status), "\n") {
		if value, found := strings.CutPrefix(line, "PPid:"); found {
			parentPID, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return codexProcess{}, fmt.Errorf("parse PPid of %d: %w", pid, err)
			}
		}
	}
	if parentPID < 0 {
		return codexProcess{}, fmt.Errorf("no PPid in /proc/%d/status", pid)
	}
	return codexProcess{
		pid:       pid,
		parentPID: parentPID,
		arguments: strings.Split(strings.TrimRight(string(commandLine), "\x00"), "\x00"),
	}, nil
}

func (procProcessTable) signal(pid int, signal syscall.Signal) error {
	return syscall.Kill(pid, signal)
}
