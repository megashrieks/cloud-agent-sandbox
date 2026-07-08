package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
)

const (
	defaultContainerName = "sandbox"
	defaultRunTimeout    = 120 * time.Second
	commandOutputLimit   = 1 << 20
	fileReadLimit        = 5 << 20
	jobLogLimit          = 1 << 20
)

// KubeExecutor runs sandbox operations through the Kubernetes pods/exec
// streaming subresource.
type KubeExecutor struct {
	cs        kubernetes.Interface
	restCfg   *rest.Config
	namespace string
	container string

	mu   sync.Mutex
	jobs map[string]*Job
}

var _ Executor = (*KubeExecutor)(nil)

// NewKubeExecutor creates an executor for pods in namespace. If containerName
// is empty, the sandbox container convention ("sandbox") is used.
func NewKubeExecutor(cs kubernetes.Interface, restCfg *rest.Config, namespace, containerName string) *KubeExecutor {
	if containerName == "" {
		containerName = defaultContainerName
	}
	return &KubeExecutor{
		cs:        cs,
		restCfg:   restCfg,
		namespace: namespace,
		container: containerName,
		jobs:      make(map[string]*Job),
	}
}

func (e *KubeExecutor) stream(ctx context.Context, podName string, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if e == nil || e.cs == nil || e.restCfg == nil {
		return errors.New("kube executor is not configured")
	}
	if podName == "" {
		return errors.New("pod name is required")
	}
	if len(argv) == 0 {
		return errors.New("command argv is required")
	}

	req := e.cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(e.namespace).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: e.container,
		Command:   argv,
		Stdin:     stdin != nil,
		Stdout:    stdout != nil,
		Stderr:    stderr != nil,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("create spdy executor: %w", err)
	}
	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    false,
	})
}

// Run executes a command synchronously and returns captured stdout/stderr. A
// non-zero process exit is represented in Result.ExitCode, not as a Go error.
func (e *KubeExecutor) Run(ctx context.Context, cmd Command) (*Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := cmd.Timeout
	if timeout == 0 {
		timeout = defaultRunTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv, err := commandArgv(cmd)
	if err != nil {
		return nil, err
	}

	stdout := newCapBuffer(commandOutputLimit)
	stderr := newCapBuffer(commandOutputLimit)
	var stdin io.Reader
	if cmd.Stdin != nil {
		stdin = bytes.NewReader(cmd.Stdin)
	}

	streamErr := e.stream(runCtx, cmd.PodName, argv, stdin, stdout, stderr)
	result := &Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  0,
		TimedOut:  errors.Is(runCtx.Err(), context.DeadlineExceeded),
		Truncated: stdout.Truncated() || stderr.Truncated(),
	}
	if streamErr == nil {
		return result, nil
	}
	if exitCode, ok := exitStatus(streamErr); ok {
		result.ExitCode = exitCode
		return result, nil
	}
	if result.TimedOut {
		return result, nil
	}
	return result, streamErr
}

// StartJob launches a shell command in the background and remembers the pod/PID
// in memory for later PollJob/StopJob calls.
func (e *KubeExecutor) StartJob(ctx context.Context, cmd Command) (*Job, error) {
	line, err := commandLine(cmd)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	logFile := "/tmp/job-" + id + ".log"
	launcher := "nohup /bin/sh -lc " + shellQuote(line) + " > " + shellQuote(logFile) + " 2>&1 & echo $!"

	res, err := e.Run(ctx, Command{PodName: cmd.PodName, Line: launcher, Timeout: cmd.Timeout})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("start job failed with exit %d: %s", res.ExitCode, res.Stderr)
	}
	pid, err := parsePID(res.Stdout)
	if err != nil {
		return nil, err
	}

	job := &Job{ID: id, PodName: cmd.PodName, PID: pid, LogFile: logFile, Running: true, StartedAt: time.Now()}
	e.mu.Lock()
	e.jobs[id] = cloneJob(job)
	e.mu.Unlock()
	return cloneJob(job), nil
}

// PollJob returns the latest remembered status plus the current job log.
func (e *KubeExecutor) PollJob(ctx context.Context, podName, jobID string) (*Job, string, error) {
	job, err := e.lookupJob(podName, jobID)
	if err != nil {
		return nil, "", err
	}

	alive, err := e.processAlive(ctx, job.PodName, job.PID)
	if err != nil {
		return nil, "", err
	}
	job.Running = alive
	if !alive {
		job.ExitCode = 0
	}

	logOutput, err := e.readJobLog(ctx, job.PodName, job.LogFile)
	if err != nil {
		return nil, "", err
	}

	e.mu.Lock()
	e.jobs[job.ID] = cloneJob(job)
	e.mu.Unlock()
	return cloneJob(job), logOutput, nil
}

// StopJob terminates a remembered background job, or best-effort interrupts
// child processes of PID 1 when no jobID is provided.
func (e *KubeExecutor) StopJob(ctx context.Context, podName, jobID string) error {
	if jobID == "" {
		_, _ = e.Run(ctx, Command{PodName: podName, Line: "pkill -INT -P 1 2>/dev/null; true", Timeout: 5 * time.Second})
		return nil
	}

	job, err := e.lookupJob(podName, jobID)
	if err != nil {
		return err
	}
	res, err := e.Run(ctx, Command{PodName: job.PodName, Line: "kill -TERM " + strconv.Itoa(job.PID) + " 2>/dev/null; true", Timeout: 5 * time.Second})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("stop job failed with exit %d: %s", res.ExitCode, res.Stderr)
	}

	e.mu.Lock()
	stored := e.jobs[jobID]
	if stored != nil {
		stored.Running = false
		stored.ExitCode = 0
	}
	e.mu.Unlock()
	return nil
}

// ReadFile streams a file out of the sandbox. Files larger than the read cap
// return an error instead of silently truncating.
func (e *KubeExecutor) ReadFile(ctx context.Context, podName, path string) ([]byte, error) {
	out := newCapBuffer(fileReadLimit)
	errOut := newCapBuffer(commandOutputLimit)
	err := e.stream(ctx, podName, []string{"/bin/sh", "-lc", "cat " + shellQuote(path)}, nil, out, errOut)
	if exitCode, ok := exitStatus(err); ok {
		return nil, fmt.Errorf("read file %q failed with exit %d: %s", path, exitCode, errOut.String())
	}
	if err != nil {
		return nil, err
	}
	if out.Truncated() {
		return nil, fmt.Errorf("read file %q exceeds %d byte limit", path, fileReadLimit)
	}
	return out.Bytes(), nil
}

// WriteFile writes content to path, creating parent directories as needed.
func (e *KubeExecutor) WriteFile(ctx context.Context, podName, path string, content []byte) error {
	cmd := "mkdir -p $(dirname " + shellQuote(path) + ") && cat > " + shellQuote(path)
	errOut := newCapBuffer(commandOutputLimit)
	err := e.stream(ctx, podName, []string{"/bin/sh", "-lc", cmd}, bytes.NewReader(content), io.Discard, errOut)
	if exitCode, ok := exitStatus(err); ok {
		return fmt.Errorf("write file %q failed with exit %d: %s", path, exitCode, errOut.String())
	}
	return err
}

// ListDir lists direct children of path using find output that is simple to
// parse: type, size, and name.
func (e *KubeExecutor) ListDir(ctx context.Context, podName, path string) ([]FileInfo, error) {
	line := "find " + shellQuote(path) + " -mindepth 1 -maxdepth 1 -printf '%y %s %f\\n'"
	res, err := e.Run(ctx, Command{PodName: podName, Line: line, Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("list dir %q failed with exit %d: %s", path, res.ExitCode, res.Stderr)
	}
	return parseFindList(res.Stdout), nil
}

func commandArgv(cmd Command) ([]string, error) {
	if cmd.Line != "" {
		line := cmd.Line
		if cmd.Cwd != "" {
			line = "cd " + shellQuote(cmd.Cwd) + " && " + line
		}
		return []string{"/bin/sh", "-lc", line}, nil
	}
	if len(cmd.Argv) == 0 {
		return nil, errors.New("command line or argv is required")
	}
	if cmd.Cwd == "" {
		return append([]string(nil), cmd.Argv...), nil
	}
	return []string{"/bin/sh", "-lc", "cd " + shellQuote(cmd.Cwd) + " && exec " + shellJoin(cmd.Argv)}, nil
}

func commandLine(cmd Command) (string, error) {
	var line string
	if cmd.Line != "" {
		line = cmd.Line
	} else if len(cmd.Argv) > 0 {
		line = shellJoin(cmd.Argv)
	} else {
		return "", errors.New("command line or argv is required")
	}
	if cmd.Cwd != "" {
		line = "cd " + shellQuote(cmd.Cwd) + " && " + line
	}
	return line, nil
}

func exitStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var exitErr k8sexec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus(), true
	}
	return 0, false
}

func shellJoin(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func parsePID(out string) (int, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return 0, errors.New("job launcher did not return a pid")
	}
	pid, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid job pid %q", fields[len(fields)-1])
	}
	return pid, nil
}

func parseFindList(out string) []FileInfo {
	var infos []FileInfo
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			continue
		}
		size, _ := strconv.ParseInt(parts[1], 10, 64)
		infos = append(infos, FileInfo{Name: parts[2], IsDir: parts[0] == "d", Size: size})
	}
	return infos
}

func (e *KubeExecutor) lookupJob(podName, jobID string) (*Job, error) {
	if jobID == "" {
		return nil, errors.New("job id is required")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	job := e.jobs[jobID]
	if job == nil {
		return nil, fmt.Errorf("unknown job %q", jobID)
	}
	if podName != "" && podName != job.PodName {
		return nil, fmt.Errorf("job %q belongs to pod %q, not %q", jobID, job.PodName, podName)
	}
	return cloneJob(job), nil
}

func (e *KubeExecutor) processAlive(ctx context.Context, podName string, pid int) (bool, error) {
	res, err := e.Run(ctx, Command{PodName: podName, Line: "kill -0 " + strconv.Itoa(pid) + " 2>/dev/null", Timeout: 5 * time.Second})
	if err != nil {
		return false, err
	}
	return res.ExitCode == 0, nil
}

func (e *KubeExecutor) readJobLog(ctx context.Context, podName, logFile string) (string, error) {
	out := newTailBuffer(jobLogLimit)
	errOut := newCapBuffer(commandOutputLimit)
	err := e.stream(ctx, podName, []string{"/bin/sh", "-lc", "cat " + shellQuote(logFile) + " 2>/dev/null || true"}, nil, out, errOut)
	if exitCode, ok := exitStatus(err); ok {
		return "", fmt.Errorf("read job log failed with exit %d: %s", exitCode, errOut.String())
	}
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

func cloneJob(job *Job) *Job {
	if job == nil {
		return nil
	}
	copy := *job
	return &copy
}

type capBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCapBuffer(limit int) *capBuffer {
	return &capBuffer{limit: limit}
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) <= remaining {
			_, _ = b.buf.Write(p)
		} else {
			_, _ = b.buf.Write(p[:remaining])
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *capBuffer) String() string  { return b.buf.String() }
func (b *capBuffer) Bytes() []byte   { return append([]byte(nil), b.buf.Bytes()...) }
func (b *capBuffer) Truncated() bool { return b.truncated }

type tailBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func newTailBuffer(limit int) *tailBuffer {
	return &tailBuffer{limit: limit}
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.truncated = true
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string  { return string(b.buf) }
func (b *tailBuffer) Truncated() bool { return b.truncated }
