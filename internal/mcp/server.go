package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/megashrieks/sandbox-orchestrator/internal/exec"
	"github.com/megashrieks/sandbox-orchestrator/internal/manager"
	"github.com/megashrieks/sandbox-orchestrator/internal/session"
)

// Server exposes sandbox operations as Model Context Protocol tools.
type Server struct {
	m   *manager.Manager
	ex  exec.Executor
	log *slog.Logger
}

// New constructs an MCP server facade for sandbox sessions.
func New(m *manager.Manager, ex exec.Executor, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{m: m, ex: ex, log: log}
}

// Handler builds the MCP server, registers sandbox tools, and returns a streamable HTTP handler.
func (s *Server) Handler() http.Handler {
	mcpSrv := mcpserver.NewMCPServer("sandbox-orchestrator", "0.1.0")
	s.registerTools(mcpSrv)
	return mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithStreamableHTTPLogger(s.log))
}

func (s *Server) registerTools(srv *mcpserver.MCPServer) {
	srv.AddTool(mcplib.NewTool("create_session",
		mcplib.WithDescription("Create one new sandbox session. Use first; other tools require the returned session_id."),
		mcplib.WithString("image", mcplib.Description("Optional container image. Omit or use \"default\" for the standard polyglot sandbox image. Otherwise pass any fully-qualified image reference the cluster can pull (e.g. alpine:3.20, python:3.12, node:22, ubuntu:24.04). By default the sandbox runs as root with a writable root filesystem so you can install packages inside it (apk add / apt-get install / pip install), and all outbound network works via the credential-injecting proxy.")),
		mcplib.WithBoolean("use_kata", mcplib.Description("Request stronger Kata isolation when available.")),
		mcplib.WithBoolean("writable_root", mcplib.Description("Optional. Default true. Whether the container root filesystem is writable so system package managers can install. Set false to harden (read-only root; /workspace and /tmp stay writable).")),
		mcplib.WithBoolean("run_as_root", mcplib.Description("Optional. Default true. Whether the sandbox process runs as root (UID 0) so it can install system packages. Set false to run as an unprivileged user.")),
	), s.createSession)

	srv.AddTool(mcplib.NewTool("shell",
		mcplib.WithDescription("Run a shell command synchronously in a sandbox and return exit code, stdout, and stderr."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("command", mcplib.Required(), mcplib.Description("Shell command line to execute.")),
		mcplib.WithString("cwd", mcplib.Description("Optional working directory.")),
		mcplib.WithNumber("timeout_seconds", mcplib.Description("Optional execution timeout in seconds.")),
	), s.shell)

	srv.AddTool(mcplib.NewTool("shell_async",
		mcplib.WithDescription("Start a long-running shell command and return a job_id for polling or stopping."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("command", mcplib.Required(), mcplib.Description("Shell command line to start.")),
		mcplib.WithString("cwd", mcplib.Description("Optional working directory.")),
	), s.shellAsync)

	srv.AddTool(mcplib.NewTool("shell_poll",
		mcplib.WithDescription("Poll an async shell job and return running/exit status plus accumulated output."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("job_id", mcplib.Required(), mcplib.Description("Job id returned by shell_async.")),
	), s.shellPoll)

	srv.AddTool(mcplib.NewTool("shell_stop",
		mcplib.WithDescription("Stop an async shell job; omit job_id to interrupt/reset the current shell."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("job_id", mcplib.Description("Optional job id to stop.")),
	), s.shellStop)

	srv.AddTool(mcplib.NewTool("shell_wait",
		mcplib.WithDescription("Block until an async shell job finishes (or the timeout elapses) and return its final status plus full accumulated output. Use after shell_async when you just want the result and don't need to poll manually."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("job_id", mcplib.Required(), mcplib.Description("Job id returned by shell_async.")),
		mcplib.WithNumber("timeout_seconds", mcplib.Description("Optional max seconds to wait. If the job is still running when this elapses, returns the current (running) status and output so far. Omit or <=0 to wait indefinitely (up to the request deadline).")),
		mcplib.WithNumber("poll_interval_seconds", mcplib.Description("Optional seconds between internal status checks. Default 1.")),
	), s.shellWait)

	srv.AddTool(mcplib.NewTool("str_replace_based_edit_tool",
		mcplib.WithDescription("File editor for a sandbox. Choose the operation with the `command` field: `view` prints a file with 1-indexed line numbers, optionally sliced by `view_range`; `create` writes (or overwrites) a file with `file_text`; `str_replace` replaces a unique `old_str` with `new_str`; `insert` adds `new_str` after line `insert_line` (0 = start of file)."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("command", mcplib.Required(), mcplib.Enum("view", "create", "str_replace", "insert"), mcplib.Description("The edit operation to perform: view | create | str_replace | insert.")),
		mcplib.WithString("path", mcplib.Required(), mcplib.Description("Absolute file path inside the sandbox (e.g. /workspace/app/main.py).")),
		mcplib.WithArray("view_range", mcplib.Description("Optional for `view`: [start_line, end_line], both 1-indexed. Use -1 as end_line to read to end of file."), mcplib.Items(map[string]any{"type": "integer"})),
		mcplib.WithString("file_text", mcplib.Description("Required for `create`: the exact, full content of the file to write.")),
		mcplib.WithString("old_str", mcplib.Description("Required for `str_replace`: the existing text to replace. Must occur exactly once in the file.")),
		mcplib.WithString("new_str", mcplib.Description("The replacement text for `str_replace`, or the text to add for `insert`.")),
		mcplib.WithNumber("insert_line", mcplib.Description("Required for `insert`: the 1-indexed line number after which to insert new_str; 0 inserts at the start of the file.")),
	), s.editTool)
}

func (s *Server) editTool(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, done, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	defer done()
	command, err := req.RequireString("command")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	switch command {
	case "view":
		content, rerr := s.ex.ReadFile(ctx, sess.PodName, path)
		if rerr != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("view: %v", rerr)), nil
		}
		start, end := viewRangeFromRequest(req)
		return mcplib.NewToolResultText(renderNumberedLines(string(content), start, end)), nil

	case "create":
		fileText, ferr := req.RequireString("file_text")
		if ferr != nil {
			return mcplib.NewToolResultError("create requires file_text"), nil
		}
		if werr := s.ex.WriteFile(ctx, sess.PodName, path, []byte(fileText)); werr != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("create: %v", werr)), nil
		}
		return mcplib.NewToolResultText(fmt.Sprintf("wrote %d bytes to %s", len(fileText), path)), nil

	case "str_replace":
		oldStr, oerr := req.RequireString("old_str")
		if oerr != nil {
			return mcplib.NewToolResultError("str_replace requires old_str"), nil
		}
		newStr := req.GetString("new_str", "")
		content, rerr := s.ex.ReadFile(ctx, sess.PodName, path)
		if rerr != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("str_replace read: %v", rerr)), nil
		}
		replaced, rperr := replaceUnique(string(content), oldStr, newStr)
		if rperr != nil {
			return mcplib.NewToolResultError(rperr.Error()), nil
		}
		if werr := s.ex.WriteFile(ctx, sess.PodName, path, []byte(replaced)); werr != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("str_replace write: %v", werr)), nil
		}
		return mcplib.NewToolResultText(fmt.Sprintf("replaced text in %s", path)), nil

	case "insert":
		line, lerr := req.RequireInt("insert_line")
		if lerr != nil {
			return mcplib.NewToolResultError("insert requires insert_line"), nil
		}
		newStr, nerr := req.RequireString("new_str")
		if nerr != nil {
			return mcplib.NewToolResultError("insert requires new_str"), nil
		}
		content, rerr := s.ex.ReadFile(ctx, sess.PodName, path)
		if rerr != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("insert read: %v", rerr)), nil
		}
		inserted, ierr := insertAfterLine(string(content), line, newStr)
		if ierr != nil {
			return mcplib.NewToolResultError(ierr.Error()), nil
		}
		if werr := s.ex.WriteFile(ctx, sess.PodName, path, []byte(inserted)); werr != nil {
			return mcplib.NewToolResultError(fmt.Sprintf("insert write: %v", werr)), nil
		}
		return mcplib.NewToolResultText(fmt.Sprintf("inserted %d bytes into %s", len(newStr), path)), nil

	default:
		return mcplib.NewToolResultError(fmt.Sprintf("unknown command %q (expected view|create|str_replace|insert)", command)), nil
	}
}

func (s *Server) createSession(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	image := req.GetString("image", "")
	useKata := req.GetBool("use_kata", false)

	opts := session.CreateOptions{Image: image, UseKata: useKata}
	args := req.GetArguments()
	if v, ok := args["writable_root"]; ok {
		if b, ok := v.(bool); ok {
			opts.WritableRoot = &b
		}
	}
	if v, ok := args["run_as_root"]; ok {
		if b, ok := v.(bool); ok {
			opts.RunAsRoot = &b
		}
	}

	sess, err := s.m.Create(ctx, opts)
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("create session: %v", err)), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("session_id: %s\nimage: %s", sess.ID, sess.Image)), nil
}

func (s *Server) shell(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, done, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	defer done()
	command, err := req.RequireString("command")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	res, err := s.ex.Run(ctx, exec.Command{
		PodName: sess.PodName,
		Line:    command,
		Cwd:     req.GetString("cwd", ""),
		Timeout: timeoutFromRequest(req),
	})
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("shell: %v", err)), nil
	}
	return mcplib.NewToolResultText(formatRunResult(res)), nil
}

func (s *Server) shellAsync(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, done, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	defer done()
	command, err := req.RequireString("command")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	job, err := s.ex.StartJob(ctx, exec.Command{PodName: sess.PodName, Line: command, Cwd: req.GetString("cwd", "")})
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("shell_async: %v", err)), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("job_id: %s", job.ID)), nil
}

func (s *Server) shellPoll(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, done, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	defer done()
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	job, output, err := s.ex.PollJob(ctx, sess.PodName, jobID)
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("shell_poll: %v", err)), nil
	}
	return mcplib.NewToolResultText(formatJobPoll(job, output)), nil
}

func (s *Server) shellWait(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, done, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	defer done()
	jobID, err := req.RequireString("job_id")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	interval := time.Duration(req.GetFloat("poll_interval_seconds", 1) * float64(time.Second))
	if interval <= 0 {
		interval = time.Second
	}
	// Bound the wait: an explicit timeout, else the request's own deadline.
	waitCtx := ctx
	if timeout := timeoutFromRequest(req); timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var buf strings.Builder
	var job *exec.Job
	for {
		j, out, perr := s.ex.PollJob(waitCtx, sess.PodName, jobID)
		if perr != nil {
			// Deadline/timeout hit while polling: return what we have so far.
			if waitCtx.Err() != nil {
				break
			}
			return mcplib.NewToolResultError(fmt.Sprintf("shell_wait: %v", perr)), nil
		}
		job = j
		// PollJob returns the full accumulated log each call, so replace rather
		// than append.
		buf.Reset()
		buf.WriteString(out)
		if j != nil && !j.Running {
			return mcplib.NewToolResultText(formatJobPoll(j, buf.String())), nil
		}
		select {
		case <-waitCtx.Done():
			return mcplib.NewToolResultText(formatJobPoll(job, buf.String())), nil
		case <-time.After(interval):
		}
	}
	return mcplib.NewToolResultText(formatJobPoll(job, buf.String())), nil
}

func (s *Server) shellStop(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, done, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	defer done()
	jobID := req.GetString("job_id", "")
	if err := s.ex.StopJob(ctx, sess.PodName, jobID); err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("shell_stop: %v", err)), nil
	}
	if jobID == "" {
		return mcplib.NewToolResultText("shell interrupted/reset"), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("stopped job_id: %s", jobID)), nil
}

// requireSession validates the session id, marks an in-flight MCP call
// (BeginActivity), and returns a cleanup func that MUST be deferred by the
// caller to mark the call complete (EndActivity). On failure the returned
// cleanup is a safe no-op.
func (s *Server) requireSession(ctx context.Context, req mcplib.CallToolRequest) (*session.Session, func(), *mcplib.CallToolResult) {
	noop := func() {}
	sessionID, err := req.RequireString("session_id")
	if err != nil {
		return nil, noop, mcplib.NewToolResultError(err.Error())
	}
	sess, err := s.m.Require(ctx, sessionID)
	if err != nil {
		if errors.Is(err, session.ErrInvalidSession) {
			return nil, noop, mcplib.NewToolResultError("invalid session")
		}
		return nil, noop, mcplib.NewToolResultError(fmt.Sprintf("require session: %v", err))
	}
	s.m.BeginActivity(ctx, sessionID)
	cleanup := func() { s.m.EndActivity(context.WithoutCancel(ctx), sessionID) }
	return sess, cleanup, nil
}

func timeoutFromRequest(req mcplib.CallToolRequest) time.Duration {
	seconds := req.GetFloat("timeout_seconds", 0)
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

// viewRangeFromRequest parses the optional `view_range` array ([start, end],
// both 1-indexed; end -1 or 0 means "to end of file") for the `view` command.
func viewRangeFromRequest(req mcplib.CallToolRequest) (int, int) {
	start, end := 1, 0
	raw, ok := req.GetArguments()["view_range"]
	if !ok {
		return start, end
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return start, end
	}
	toInt := func(v any) (int, bool) {
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		case int64:
			return int(n), true
		}
		return 0, false
	}
	if v, ok := toInt(arr[0]); ok {
		start = v
	}
	if len(arr) > 1 {
		if v, ok := toInt(arr[1]); ok {
			end = v // -1/0 -> renderNumberedLines treats as end of file
			if end < 0 {
				end = 0
			}
		}
	}
	return start, end
}

func formatRunResult(res *exec.Result) string {
	if res == nil {
		return "no result"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "exit_code: %d\n", res.ExitCode)
	if res.TimedOut {
		b.WriteString("timed_out: true\n")
	}
	if res.Truncated {
		b.WriteString("truncated: true\n")
	}
	fmt.Fprintf(&b, "stdout:\n---\n%s\n---\nstderr:\n---\n%s\n---", res.Stdout, res.Stderr)
	return b.String()
}

func formatJobPoll(job *exec.Job, output string) string {
	if job == nil {
		return fmt.Sprintf("job: unknown\noutput:\n---\n%s\n---", output)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "job_id: %s\nrunning: %t\n", job.ID, job.Running)
	if !job.Running {
		fmt.Fprintf(&b, "exit_code: %d\n", job.ExitCode)
	}
	fmt.Fprintf(&b, "output:\n---\n%s\n---", output)
	return b.String()
}
