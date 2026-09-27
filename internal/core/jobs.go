package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/sshx"
)

// JobInfo describes a long-running operation (deploy, compose up, backup,
// certificate issue, package install…) whose output streams to the UI.
type JobInfo struct {
	ID       string        `json:"id"`
	Server   string        `json:"server"`
	Kind     string        `json:"kind"`
	Title    string        `json:"title"`
	Started  int64         `json:"started"`  // unix ms
	Finished int64         `json:"finished"` // 0 while running
	State    string        `json:"state"`    // running | done | error | cancelled
	Error    *apperr.Error `json:"error"`
	// Log is the retained output (the tail, when very long).
	Log string `json:"log"`
	// Written counts every byte of output so far; a UI that fetched Log can
	// skip job:output events whose Offset+len(Data) <= Written.
	Written int64 `json:"written"`
	// Result is an optional value set by the job (e.g. a run id).
	Result string `json:"result"`
}

type JobOutputEvent struct {
	ID string `json:"id"`
	// Offset is the number of bytes written before Data.
	Offset int64  `json:"offset"`
	Data   string `json:"data"`
}

const jobLogMax = 2 << 20

// Job is a running operation. It is an io.Writer: everything written is
// kept (up to 2 MB, oldest dropped) and streamed to the frontend.
type Job struct {
	mu      sync.Mutex
	info    JobInfo
	log     strings.Builder
	pending strings.Builder
	pendOff int64 // offset of pending's first byte
	timer   *time.Timer
	cancel  context.CancelFunc
	done    chan struct{}
}

func (j *Job) ID() string { return j.info.ID }

func (j *Job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	s := string(p)
	j.log.WriteString(s)
	if j.log.Len() > jobLogMax {
		keep := j.log.String()[j.log.Len()-jobLogMax/2:]
		j.log.Reset()
		j.log.WriteString("…\n" + keep)
	}
	if j.pending.Len() == 0 {
		j.pendOff = j.info.Written
	}
	j.pending.WriteString(s)
	j.info.Written += int64(len(p))
	// Coalesce chatty output into ~10 events per second.
	if j.timer == nil {
		j.timer = time.AfterFunc(100*time.Millisecond, j.flush)
	}
	return len(p), nil
}

func (j *Job) flush() {
	j.mu.Lock()
	data := j.pending.String()
	off := j.pendOff
	j.pending.Reset()
	j.timer = nil
	id := j.info.ID
	j.mu.Unlock()
	if data != "" {
		Emit(EventJobOutput, JobOutputEvent{ID: id, Offset: off, Data: data})
	}
}

// Logf writes a line of the job's own narration (steps, results).
func (j *Job) Logf(format string, args ...any) {
	fmt.Fprintf(j, format+"\n", args...)
}

// Step writes a highlighted step header.
func (j *Job) Step(format string, args ...any) {
	fmt.Fprintf(j, "\x1b[1;36m▶ "+format+"\x1b[0m\n", args...)
}

// SetResult stores a value the UI can read when the job ends.
func (j *Job) SetResult(v string) {
	j.mu.Lock()
	j.info.Result = v
	j.mu.Unlock()
}

// Info snapshots the job.
func (j *Job) Info() JobInfo {
	j.mu.Lock()
	defer j.mu.Unlock()
	i := j.info
	i.Log = j.log.String()
	return i
}

// Wait blocks until the job ends.
func (j *Job) Wait() JobInfo {
	<-j.done
	return j.Info()
}

// Exec streams a remote command's output into the job, through sudo when
// requested. It returns the exit code; a non-zero code is not an error.
func (c *Core) JobExec(ctx context.Context, j *Job, conn *sshx.Conn, cmd string, sudo bool, password string) (int, error) {
	return c.JobExecInput(ctx, j, conn, cmd, sudo, password, nil)
}

// JobExecInput is JobExec with stdin.
func (c *Core) JobExecInput(ctx context.Context, j *Job, conn *sshx.Conn, cmd string, sudo bool, password string, stdin io.Reader) (int, error) {
	full := "sh -c " + sshx.ShellQuote(cmd)
	var in io.Reader = stdin
	var errBuf tailWriter
	if sudo && !c.IsRoot(ctx, conn) {
		pw := c.SudoPassword(conn, password)
		f, input, err := sudoCommand(ctx, conn, pw, cmd, "")
		if err != nil {
			return -1, err
		}
		full = f
		if input != "" {
			if stdin != nil {
				in = io.MultiReader(strings.NewReader(input), stdin)
			} else {
				in = strings.NewReader(input)
			}
		}
		code, err := conn.Stream(ctx, full, in, j, io.MultiWriter(j, &errBuf))
		if err == nil && code != 0 {
			if e := sudoError(errBuf.String()); e != nil {
				return code, e
			}
		}
		if err == nil {
			c.rememberSudo(conn, pw)
		}
		return code, err
	}
	return conn.Stream(ctx, full, in, j, j)
}

// JobRun is JobExec that fails on a non-zero exit code.
func (c *Core) JobRun(ctx context.Context, j *Job, conn *sshx.Conn, cmd string, sudo bool, password string) error {
	code, err := c.JobExec(ctx, j, conn, cmd, sudo, password)
	if err != nil {
		return err
	}
	if code != 0 {
		return apperr.New("cmd.failed", "code", fmt.Sprint(code))
	}
	return nil
}

// tailWriter keeps the last 4 KB written (to inspect sudo errors).
type tailWriter struct{ b []byte }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 4096 {
		t.b = t.b[len(t.b)-4096:]
	}
	return len(p), nil
}
func (t *tailWriter) String() string { return string(t.b) }

// Jobs tracks running and recently finished jobs.
type Jobs struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func newJobs() *Jobs { return &Jobs{jobs: map[string]*Job{}} }

// Start runs fn in the background as a job. fn's ctx is cancelled by
// Cancel; its returned error becomes the job's error.
func (js *Jobs) Start(server, kind, title string, fn func(ctx context.Context, j *Job) error) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{
		info:   JobInfo{ID: uuid.NewString(), Server: server, Kind: kind, Title: title, Started: time.Now().UnixMilli(), State: "running"},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	js.mu.Lock()
	js.jobs[j.info.ID] = j
	js.gcLocked()
	js.mu.Unlock()
	go func() {
		defer cancel()
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic: %v", r)
				}
			}()
			err = fn(ctx, j)
		}()
		j.mu.Lock()
		if j.timer != nil {
			j.timer.Stop()
		}
		j.mu.Unlock()
		j.flush()
		j.mu.Lock()
		j.info.Finished = time.Now().UnixMilli()
		switch {
		case err == nil:
			j.info.State = "done"
		case errors.Is(err, context.Canceled) || ctx.Err() != nil:
			j.info.State = "cancelled"
		default:
			j.info.State = "error"
			j.info.Error = apperr.From(err)
		}
		info := j.info
		j.mu.Unlock()
		close(j.done)
		Emit(EventJobDone, info)
	}()
	return j
}

// gcLocked forgets finished jobs beyond the 100 most recent.
func (js *Jobs) gcLocked() {
	if len(js.jobs) <= 100 {
		return
	}
	var finished []*Job
	for _, j := range js.jobs {
		if j.info.Finished > 0 {
			finished = append(finished, j)
		}
	}
	sort.Slice(finished, func(a, b int) bool { return finished[a].info.Finished < finished[b].info.Finished })
	for i := 0; i < len(finished) && len(js.jobs) > 100; i++ {
		delete(js.jobs, finished[i].info.ID)
	}
}

func (js *Jobs) Get(id string) (*Job, bool) {
	js.mu.Lock()
	defer js.mu.Unlock()
	j, ok := js.jobs[id]
	return j, ok
}

func (js *Jobs) Cancel(id string) {
	if j, ok := js.Get(id); ok {
		j.cancel()
	}
}

func (js *Jobs) CancelAll() {
	js.mu.Lock()
	defer js.mu.Unlock()
	for _, j := range js.jobs {
		j.cancel()
	}
}

// List returns jobs (without logs), newest first; server "" = all.
func (js *Jobs) List(server string) []JobInfo {
	js.mu.Lock()
	defer js.mu.Unlock()
	out := []JobInfo{}
	for _, j := range js.jobs {
		j.mu.Lock()
		i := j.info
		j.mu.Unlock()
		if server == "" || i.Server == server {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Started > out[b].Started })
	return out
}
