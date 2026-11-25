package container

import (
	"strconv"
	"strings"
)

const maxList = 64 // containers kept of one docker ps

// Entry is one container of the host as `docker ps -a` lists it. Only the name,
// state and what its status text says are kept.
type Entry struct {
	Name     string // cleaned, at most maxString bytes
	State    string // "running", "exited", "created", "restarting", "paused", "dead", "removing"
	Status   string // docker's status text, e.g. "Up 2 hours (healthy)", cleaned
	Health   string // from Status: "healthy", "unhealthy", "starting", or ""
	ExitCode int    // from Status, when HasExit
	HasExit  bool
	Llama    bool // publishes the dashboard's port
}

// Level is the status light of a container.
type Level int

const (
	LevelOff      Level = iota // stopped on purpose, or a state not known
	LevelOK                    // running, healthy or without a healthcheck
	LevelStarting              // health starting, or restarting
	LevelFailed                // dead, unhealthy, or exited with an error code
)

func (l Level) String() string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelStarting:
		return "starting"
	case LevelFailed:
		return "failed"
	}
	return "off"
}

// Classify is the level of a container in state with health and, for an exited
// one, its exit code. Exit codes 0, 137 (killed, as `docker stop` does after its
// timeout) and 143 (terminated) are a stop, not a failure; an exit code that is
// not known is too. Unknown states are off.
func Classify(state, health string, exitCode int, hasExit bool) Level {
	switch state {
	case "running":
		switch health {
		case "starting":
			return LevelStarting
		case "unhealthy":
			return LevelFailed
		}
		return LevelOK
	case "restarting":
		return LevelStarting
	case "dead":
		return LevelFailed
	case "exited":
		if hasExit && exitCode != 0 && exitCode != 137 && exitCode != 143 {
			return LevelFailed
		}
	}
	return LevelOff
}

// Level is Classify of e.
func (e Entry) Level() Level { return Classify(e.State, e.Health, e.ExitCode, e.HasExit) }

// parseHealth reads the health docker appends to a status text.
func parseHealth(status string) string {
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	}
	return ""
}

// parseExit reads the code of an "Exited (N) ..." status text.
func parseExit(status string) (int, bool) {
	rest, ok := strings.CutPrefix(status, "Exited (")
	if !ok {
		return 0, false
	}
	num, _, ok := strings.Cut(rest, ")")
	if !ok || num == "" || len(num) > 4 {
		return 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
