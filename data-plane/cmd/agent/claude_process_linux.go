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
// caller rather than an unrelated namespace init. Each invocation's reaper
// helper calls it for itself, which is what keeps one invocation's detached
// tools reachable by that invocation and no other; the agent calls it as well,
// so a tool that outlives its reaper still lands on a live process instead of
// leaking out of the pod's process tree.
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

// killAndReapOwnDescendants kills and waits on every process that has
// reparented onto the caller.
//
// It waits on any child, so it may only run in a process whose children all
// belong to one invocation — the claude-reaper helper. Running it in the agent
// is what the manager pod's two lanes could not survive: the agent runs their
// invocations concurrently on one execCommandRunner, so the lane that finished
// first killed the other lane's CLI and took its exit status.
func killAndReapOwnDescendants() error {
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

// reclaimClaudeSession kills and waits on whatever is left of one invocation's
// session. The agent gives every invocation its own session (Setsid in
// execCommandRunner.Run) and the session id is the reaper's pid, so this
// reaches that invocation's processes and nothing else: no sibling lane's CLI,
// and no status another wait is owed.
//
// It is the agent's backstop for the one path where the reaper does not clean
// up after itself — its WaitDelay kill. A tool that called setsid for itself
// left this session and is left to the agent's own subreaper adoption, which
// is why prepareClaudeProcessIsolation still runs in the agent.
func reclaimClaudeSession(sid int) error {
	if sid <= 1 {
		return fmt.Errorf("refusing to reclaim claude session %d", sid)
	}
	deadline := time.Now().Add(claudeDescendantKillGrace)
	var errs []error
	for {
		members, err := claudeSessionMembers(sid)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		if len(members) == 0 {
			return errors.Join(errs...)
		}
		for _, pid := range members {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				errs = append(errs, fmt.Errorf("kill leftover claude process %d: %w", pid, err))
			}
		}
		// Waited on by pid, never on -1: a sibling lane's invocation is a child
		// of this same agent, and a wait on any child would take its status.
		for _, pid := range members {
			var status syscall.WaitStatus
			if _, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); err != nil &&
				!errors.Is(err, syscall.ECHILD) {
				errs = append(errs, fmt.Errorf("reap leftover claude process %d: %w", pid, err))
			}
		}
		if time.Now().After(deadline) {
			errs = append(errs, errors.New("timed out reclaiming the claude invocation session"))
			return errors.Join(errs...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func directClaudeChildren(parentPID int) ([]int, error) {
	return scanClaudeProcesses(func(stat claudeProcStat) bool { return stat.ppid == parentPID })
}

func claudeSessionMembers(sid int) ([]int, error) {
	return scanClaudeProcesses(func(stat claudeProcStat) bool { return stat.session == sid })
}

// claudeProcStat is the part of /proc/<pid>/stat this file matches on.
type claudeProcStat struct {
	pid     int
	ppid    int
	pgrp    int
	session int
}

func scanClaudeProcesses(match func(claudeProcStat) bool) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("scan /proc for claude processes: %w", err)
	}
	self := os.Getpid()
	var matched []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, ok := parseClaudeProcStat(pid, data)
		if ok && match(stat) {
			matched = append(matched, pid)
		}
	}
	return matched, nil
}

func parseClaudeProcStat(pid int, data []byte) (claudeProcStat, bool) {
	// comm is parenthesized and may contain spaces or ')', so split only after
	// its final ')' before reading state, PPID, process group and session.
	endComm := bytes.LastIndexByte(data, ')')
	if endComm < 0 {
		return claudeProcStat{}, false
	}
	fields := strings.Fields(string(data[endComm+1:]))
	if len(fields) < 4 {
		return claudeProcStat{}, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return claudeProcStat{}, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return claudeProcStat{}, false
	}
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return claudeProcStat{}, false
	}
	return claudeProcStat{pid: pid, ppid: ppid, pgrp: pgrp, session: session}, true
}
