package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	prSetChildSubreaper       = 36
	claudeDescendantKillGrace = 2 * time.Second
)

// prepareClaudeProcessIsolation makes orphaned grandchildren reparent to the
// agent rather than an unrelated namespace init. It complements the per-run
// process group: tools that explicitly call setsid still become discoverable
// direct children after the CLI exits.
func prepareClaudeProcessIsolation() error {
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_PRCTL,
		uintptr(prSetChildSubreaper),
		uintptr(1),
		0, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("enable claude child subreaper: %w", errno)
	}
	return nil
}

// killAndReapClaudeDescendants runs only after cmd.Wait has reaped the direct
// CLI process. The Claude worker is the sole subprocess owner in this workload
// mode, so any remaining descendants are detached tools from that invocation.
func killAndReapClaudeDescendants() error {
	deadline := time.Now().Add(claudeDescendantKillGrace)
	var errs []error
	for {
		children, err := directClaudeChildren(os.Getpid())
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		for _, pid := range children {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("kill detached claude child %d: %w", pid, err))
			}
		}
		for {
			var status syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
			if errors.Is(err, syscall.ECHILD) || pid == 0 {
				break
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("reap detached claude child: %w", err))
				break
			}
		}
		if len(children) == 0 {
			return errors.Join(errs...)
		}
		if time.Now().After(deadline) {
			errs = append(errs, errors.New("timed out cleaning detached claude children"))
			return errors.Join(errs...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func directClaudeChildren(parentPID int) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("scan /proc for claude children: %w", err)
	}
	var children []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == parentPID {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// comm is parenthesized and may contain spaces or ')', so split only
		// after its final ')' before reading state and PPID.
		endComm := bytes.LastIndexByte(data, ')')
		if endComm < 0 {
			continue
		}
		fields := strings.Fields(string(data[endComm+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err == nil && ppid == parentPID {
			children = append(children, pid)
		}
	}
	return children, nil
}
