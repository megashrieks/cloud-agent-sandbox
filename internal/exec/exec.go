// Package exec defines the contract for running commands and moving files in a
// sandbox. All operations execute from the trusted orchestrator via the
// Kubernetes pods/exec streaming subresource, so no trusted agent runs inside
// the untrusted sandbox.
package exec

import (
	"context"
	"time"
)

// Command describes a synchronous command execution request.
type Command struct {
	// PodName is the target sandbox pod.
	PodName string
	// Argv is the command and its arguments. When using the persistent shell,
	// the executor may instead accept a single command line.
	Argv []string
	// Line is a raw shell command line, executed in the session's persistent
	// stateful shell when non-empty (takes precedence over Argv).
	Line string
	// Cwd overrides the working directory for this command (best-effort).
	Cwd string
	// Timeout bounds the execution; zero means the executor default.
	Timeout time.Duration
	// Stdin is optional input piped to the command.
	Stdin []byte
}

// Result is the outcome of a synchronous command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	// TimedOut is true if the command was killed due to Timeout.
	TimedOut bool
	// Truncated is true if output was capped.
	Truncated bool
}

// Job is a handle to a background (async) command.
type Job struct {
	ID      string
	PodName string
	PID     int
	LogFile string
	Running bool
	ExitCode int
	StartedAt time.Time
}

// FileInfo is a single directory entry.
type FileInfo struct {
	Name  string
	IsDir bool
	Size  int64
}

// Executor runs commands and moves files in sandbox pods.
type Executor interface {
	// Run executes a command synchronously and returns its result.
	Run(ctx context.Context, cmd Command) (*Result, error)

	// StartJob launches a command in the background (in the persistent shell)
	// and returns a job handle immediately.
	StartJob(ctx context.Context, cmd Command) (*Job, error)
	// PollJob returns current status plus any new output since the last poll.
	PollJob(ctx context.Context, podName, jobID string) (*Job, string, error)
	// StopJob terminates a background job. If jobID is empty, it interrupts the
	// current foreground command / resets the persistent shell.
	StopJob(ctx context.Context, podName, jobID string) error

	// ReadFile streams a file out of the sandbox.
	ReadFile(ctx context.Context, podName, path string) ([]byte, error)
	// WriteFile writes content to a path in the sandbox (create/overwrite).
	WriteFile(ctx context.Context, podName, path string, content []byte) error
	// ListDir lists a directory in the sandbox.
	ListDir(ctx context.Context, podName, path string) ([]FileInfo, error)
}
