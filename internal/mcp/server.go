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
		mcplib.WithString("image", mcplib.Description("Optional container image override.")),
		mcplib.WithBoolean("use_kata", mcplib.Description("Request stronger Kata isolation when available.")),
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

	srv.AddTool(mcplib.NewTool("read_file",
		mcplib.WithDescription("Read a sandbox file and return 1-indexed numbered lines, optionally sliced by line range."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("path", mcplib.Required(), mcplib.Description("File path inside the sandbox.")),
		mcplib.WithNumber("start_line", mcplib.Description("Optional first line to include, 1-indexed.")),
		mcplib.WithNumber("end_line", mcplib.Description("Optional last line to include, inclusive.")),
	), s.readFile)

	srv.AddTool(mcplib.NewTool("write_file",
		mcplib.WithDescription("Create or overwrite a sandbox file with exact content."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("path", mcplib.Required(), mcplib.Description("File path inside the sandbox.")),
		mcplib.WithString("content", mcplib.Required(), mcplib.Description("Exact file content to write.")),
	), s.writeFile)

	srv.AddTool(mcplib.NewTool("str_replace",
		mcplib.WithDescription("Replace old_str with new_str in a file only when old_str occurs exactly once."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("path", mcplib.Required(), mcplib.Description("File path inside the sandbox.")),
		mcplib.WithString("old_str", mcplib.Required(), mcplib.Description("Existing text that must match exactly once.")),
		mcplib.WithString("new_str", mcplib.Required(), mcplib.Description("Replacement text.")),
	), s.strReplace)

	srv.AddTool(mcplib.NewTool("insert",
		mcplib.WithDescription("Insert text after a 1-indexed line in a file; line 0 prepends."),
		mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("Sandbox session id from create_session.")),
		mcplib.WithString("path", mcplib.Required(), mcplib.Description("File path inside the sandbox.")),
		mcplib.WithNumber("line", mcplib.Required(), mcplib.Description("Line after which to insert; 0 prepends.")),
		mcplib.WithString("text", mcplib.Required(), mcplib.Description("Exact text to insert.")),
	), s.insert)
}

func (s *Server) createSession(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	image := req.GetString("image", "")
	useKata := req.GetBool("use_kata", false)

	sess, err := s.m.Create(ctx, session.CreateOptions{Image: image, UseKata: useKata})
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("create session: %v", err)), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("session_id: %s\nimage: %s", sess.ID, sess.Image)), nil
}

func (s *Server) shell(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
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
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
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
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
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

func (s *Server) shellStop(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	jobID := req.GetString("job_id", "")
	if err := s.ex.StopJob(ctx, sess.PodName, jobID); err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("shell_stop: %v", err)), nil
	}
	if jobID == "" {
		return mcplib.NewToolResultText("shell interrupted/reset"), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("stopped job_id: %s", jobID)), nil
}

func (s *Server) readFile(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	content, err := s.ex.ReadFile(ctx, sess.PodName, path)
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("read_file: %v", err)), nil
	}
	start, end := lineRangeFromRequest(req)
	return mcplib.NewToolResultText(renderNumberedLines(string(content), start, end)), nil
}

func (s *Server) writeFile(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	content, err := req.RequireString("content")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	if err := s.ex.WriteFile(ctx, sess.PodName, path, []byte(content)); err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("write_file: %v", err)), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("wrote %d bytes to %s", len([]byte(content)), path)), nil
}

func (s *Server) strReplace(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	oldStr, err := req.RequireString("old_str")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	newStr, err := req.RequireString("new_str")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	content, err := s.ex.ReadFile(ctx, sess.PodName, path)
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("str_replace read: %v", err)), nil
	}
	replaced, err := replaceUnique(string(content), oldStr, newStr)
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	if err := s.ex.WriteFile(ctx, sess.PodName, path, []byte(replaced)); err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("str_replace write: %v", err)), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("replaced text in %s", path)), nil
}

func (s *Server) insert(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
	sess, fail := s.requireSession(ctx, req)
	if fail != nil {
		return fail, nil
	}
	path, err := req.RequireString("path")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	line, err := req.RequireInt("line")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	text, err := req.RequireString("text")
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}

	content, err := s.ex.ReadFile(ctx, sess.PodName, path)
	if err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("insert read: %v", err)), nil
	}
	inserted, err := insertAfterLine(string(content), line, text)
	if err != nil {
		return mcplib.NewToolResultError(err.Error()), nil
	}
	if err := s.ex.WriteFile(ctx, sess.PodName, path, []byte(inserted)); err != nil {
		return mcplib.NewToolResultError(fmt.Sprintf("insert write: %v", err)), nil
	}
	return mcplib.NewToolResultText(fmt.Sprintf("inserted %d bytes into %s", len([]byte(text)), path)), nil
}

func (s *Server) requireSession(ctx context.Context, req mcplib.CallToolRequest) (*session.Session, *mcplib.CallToolResult) {
	sessionID, err := req.RequireString("session_id")
	if err != nil {
		return nil, mcplib.NewToolResultError(err.Error())
	}
	sess, err := s.m.Require(ctx, sessionID)
	if err != nil {
		if errors.Is(err, session.ErrInvalidSession) {
			return nil, mcplib.NewToolResultError("invalid session")
		}
		return nil, mcplib.NewToolResultError(fmt.Sprintf("require session: %v", err))
	}
	if err := s.m.Touch(ctx, sessionID); err != nil {
		return nil, mcplib.NewToolResultError(fmt.Sprintf("touch session: %v", err))
	}
	return sess, nil
}

func timeoutFromRequest(req mcplib.CallToolRequest) time.Duration {
	seconds := req.GetFloat("timeout_seconds", 0)
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}

func lineRangeFromRequest(req mcplib.CallToolRequest) (int, int) {
	args := req.GetArguments()
	start, end := 1, 0
	if _, ok := args["start_line"]; ok {
		start = req.GetInt("start_line", 1)
	}
	if _, ok := args["end_line"]; ok {
		end = req.GetInt("end_line", 0)
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
