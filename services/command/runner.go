package command

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// run is an active execution.
type run struct {
	mu      sync.Mutex
	info    RunInfo
	servers map[string]int // server id -> index in info.Results
	failed  bool           // a server failed (for StopOnFailure)
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

func (r *run) snapshot() RunInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.info
	info.Results = append([]ServerResult{}, r.info.Results...)
	info.Summary = summarize(info.Results)
	return info
}

// set updates one server's result and emits it.
func (r *run) set(i int, fn func(*ServerResult)) ServerResult {
	r.mu.Lock()
	fn(&r.info.Results[i])
	res := r.info.Results[i]
	if res.State == StateFailed || res.State == StateTimeout {
		r.failed = true
	}
	r.mu.Unlock()
	core.Emit(EventUpdate, res)
	return res
}

func summarize(rs []ServerResult) Summary {
	s := Summary{Total: len(rs)}
	for _, r := range rs {
		switch r.State {
		case StateQueued:
			s.Queued++
		case StateConnecting, StateRunning:
			s.Running++
		case StateDone:
			s.OK++
		case StateFailed:
			s.Failed++
		case StateTimeout:
			s.Timeout++
		case StateCancelled:
			s.Cancelled++
		case StateSkipped:
			s.Skipped++
		}
	}
	return s
}

// Run validates spec and starts executing it in the background. Progress
// arrives as command:update events and the end as command:finished.
func (s *CommandService) Run(spec Spec) (string, error) {
	p, err := s.prepare(spec)
	if err != nil {
		return "", err
	}
	return s.start(p)
}

func (s *CommandService) start(p prepared) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &run{
		info: RunInfo{
			ID:      uuid.NewString(),
			Spec:    p.spec,
			Command: p.command,
			Sudo:    p.sudo,
			Actor:   s.core.Actor(),
			Started: time.Now().UnixMilli(),
			Status:  "running",
			Dangers: p.dangers,
			Results: make([]ServerResult, len(p.servers)),
		},
		servers: map[string]int{},
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
	}
	if r.info.Dangers == nil {
		r.info.Dangers = []Danger{}
	}
	for i, sv := range p.servers {
		r.info.Results[i] = ServerResult{RunID: r.info.ID, ServerID: sv.ID, Name: sv.Name, Environment: sv.Environment, State: StateQueued}
		r.servers[sv.ID] = i
	}
	if err := s.persist(r.snapshot()); err != nil {
		cancel()
		return "", err
	}
	s.mu.Lock()
	s.runs[r.info.ID] = r
	s.mu.Unlock()
	for i := range r.info.Results {
		core.Emit(EventUpdate, r.info.Results[i])
	}
	go s.execute(r, p)
	return r.info.ID, nil
}

// execute schedules servers on a bounded worker pool.
func (s *CommandService) execute(r *run, p prepared) {
	defer close(r.done)
	sem := make(chan struct{}, p.spec.Concurrency)
	var wg sync.WaitGroup
	for i := range p.servers {
		select {
		case sem <- struct{}{}:
		case <-r.ctx.Done():
		}
		if r.ctx.Err() != nil {
			s.markRest(r, i, StateCancelled, apperr.New("cmd.cancelled"))
			break
		}
		r.mu.Lock()
		stop := p.spec.StopOnFailure && r.failed
		r.mu.Unlock()
		if stop {
			<-sem
			s.markRest(r, i, StateSkipped, apperr.New("cmd.stoppedAfterFailure"))
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if v := recover(); v != nil {
					r.set(i, func(res *ServerResult) {
						res.State = StateFailed
						res.Error = apperr.New("generic").WithDetail(fmt.Sprint(v))
						res.FinishedAt = time.Now().UnixMilli()
					})
				}
			}()
			s.runOne(r, p, i)
		}(i)
	}
	wg.Wait()
	s.finish(r)
}

func (s *CommandService) markRest(r *run, from int, state string, e *apperr.Error) {
	now := time.Now().UnixMilli()
	for j := from; j < len(r.info.Results); j++ {
		r.set(j, func(res *ServerResult) {
			if res.State == StateQueued {
				res.State = state
				res.Error = e
				res.FinishedAt = now
			}
		})
	}
}

// runOne executes the command on one server.
func (s *CommandService) runOne(r *run, p prepared, i int) {
	sv := p.servers[i]
	timeout := time.Duration(p.spec.TimeoutSec) * time.Second
	auditDetail := fmt.Sprintf("run=%s sudo=%t timeout=%ds", r.info.ID, p.sudo, p.spec.TimeoutSec)
	auditTarget := auditCommand(p)
	fail := func(state string, err error) {
		ae := apperr.From(err)
		r.set(i, func(res *ServerResult) {
			res.State = state
			res.Error = ae
			res.ExitCode = -1
			res.FinishedAt = time.Now().UnixMilli()
		})
	}

	r.set(i, func(res *ServerResult) {
		res.State = StateConnecting
		res.StartedAt = time.Now().UnixMilli()
	})
	if err := s.core.Require(sv.ID, core.PermExec); err != nil {
		fail(StateSkipped, err)
		s.core.Audit(sv.ID, "cmd.run", auditTarget, auditDetail, err)
		return
	}

	cctx, ccancel := context.WithTimeout(r.ctx, connectTimeout)
	conn, err := s.acquireConn(cctx, sv.ID)
	ccancel()
	if err != nil {
		switch {
		case r.ctx.Err() != nil:
			fail(StateCancelled, apperr.New("cmd.cancelled"))
			return
		case apperr.HasCode(err, "auth.needSecret"):
			fail(StateSkipped, err)
		default:
			fail(StateFailed, connError(err, cctx))
		}
		s.core.Audit(sv.ID, "cmd.run", auditTarget, auditDetail, err)
		return
	}
	defer s.releaseConn(sv.ID)
	if r.ctx.Err() != nil {
		fail(StateCancelled, apperr.New("cmd.cancelled"))
		return
	}

	started := time.Now()
	r.set(i, func(res *ServerResult) {
		res.State = StateRunning
		res.StartedAt = started.UnixMilli()
	})
	// The server enforces the timeout (see wrapScript); the local deadline
	// is a backstop for servers without `timeout` or a stuck channel.
	mk := marker(r.info.ID)
	ectx, ecancel := context.WithTimeout(r.ctx, timeout+10*time.Second)
	res, err := s.core.Run(ectx, conn, wrapScript(p.command, p.spec.TimeoutSec, mk), p.sudo, "", "")
	localTimeout := errors.Is(ectx.Err(), context.DeadlineExceeded)
	ecancel()
	elapsed := time.Since(started)
	if r.ctx.Err() != nil || localTimeout {
		// Closing the channel doesn't stop the remote command; kill its group.
		kctx, kcancel := context.WithTimeout(context.Background(), 15*time.Second)
		_, _ = s.core.Run(kctx, conn, killScript(mk), p.sudo, "", "")
		kcancel()
	}

	stdout, tOut := truncateUTF8(res.Stdout, liveOutputMax)
	stderr, tErr := truncateUTF8(res.Stderr, liveOutputMax)
	out := func(state string, code int, e *apperr.Error) ServerResult {
		return r.set(i, func(x *ServerResult) {
			x.State = state
			x.ExitCode = code
			x.Stdout = stdout
			x.Stderr = stderr
			x.Truncated = tOut || tErr
			x.Error = e
			x.FinishedAt = time.Now().UnixMilli()
		})
	}

	var auditErr error
	switch {
	case r.ctx.Err() != nil:
		auditErr = apperr.New("cmd.cancelled")
		out(StateCancelled, -1, apperr.New("cmd.cancelled"))
	case localTimeout || (err == nil && remoteTimedOut(res.ExitCode, elapsed, timeout)):
		auditErr = apperr.New("cmd.timeout", "sec", fmt.Sprint(p.spec.TimeoutSec))
		out(StateTimeout, res.ExitCode, apperr.New("cmd.timeout", "sec", fmt.Sprint(p.spec.TimeoutSec)))
	case err != nil:
		auditErr = err
		out(StateFailed, -1, connError(err, nil))
	case res.ExitCode == 0:
		out(StateDone, 0, nil)
	default:
		auditErr = apperr.New("cmd.failed", "code", fmt.Sprint(res.ExitCode))
		out(StateFailed, res.ExitCode, nil)
	}
	s.core.Audit(sv.ID, "cmd.run", auditTarget, auditDetail, auditErr)
}

// remoteTimedOut recognises a command killed by the server-side timeout:
// GNU timeout exits 124 (137 after KILL), BusyBox reports the signal.
func remoteTimedOut(code int, elapsed, timeout time.Duration) bool {
	if elapsed < timeout-500*time.Millisecond {
		return false
	}
	return code == 124 || code == 137 || code == 143 || code == -1
}

// connError turns connection errors into coded ones.
func connError(err error, ctx context.Context) *apperr.Error {
	var hk *sshx.HostKeyError
	if errors.As(err, &hk) {
		if hk.Mismatch {
			return apperr.New("cmd.hostKeyChanged", "host", hk.Host)
		}
		return apperr.New("cmd.hostKeyUnknown", "host", hk.Host)
	}
	if ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return apperr.New("cmd.connectTimeout")
	}
	return apperr.From(err)
}

func auditCommand(p prepared) string {
	if p.spec.Kind == "preset" {
		return "preset " + p.spec.Preset + " " + formatParams(p.spec.Params) + "\n" + p.command
	}
	return p.command
}

func formatParams(m map[string]string) string {
	parts := []string{}
	for k, v := range m {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

func truncateUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// finish records the outcome of a run.
func (s *CommandService) finish(r *run) {
	r.mu.Lock()
	r.info.Finished = time.Now().UnixMilli()
	sum := summarize(r.info.Results)
	r.info.Summary = sum
	switch {
	case r.ctx.Err() != nil:
		r.info.Status = "cancelled"
	case sum.OK == sum.Total:
		r.info.Status = "done"
	default:
		r.info.Status = "failed"
	}
	r.mu.Unlock()
	info := r.snapshot()
	r.cancel()

	if err := s.persist(info); err != nil {
		log.Printf("command: save run %s: %v", info.ID, err)
	}
	var auditErr error
	if info.Status != "done" {
		auditErr = apperr.New("cmd.runFailed", "ok", fmt.Sprint(sum.OK), "total", fmt.Sprint(sum.Total))
	}
	p := prepared{spec: info.Spec, command: info.Command}
	s.core.Audit("", "cmd.batch", auditCommand(p), fmt.Sprintf("run=%s servers=%d ok=%d failed=%d timeout=%d skipped=%d cancelled=%d sudo=%t timeout=%ds",
		info.ID, sum.Total, sum.OK, sum.Failed, sum.Timeout, sum.Skipped, sum.Cancelled, info.Sudo, info.Spec.TimeoutSec), auditErr)

	s.mu.Lock()
	delete(s.runs, info.ID)
	s.mu.Unlock()
	core.Emit(EventFinished, FinishedEvent{RunID: info.ID, Status: info.Status, Summary: info.Summary, Finished: info.Finished})
}

// Cancel stops a run: queued servers are not started and running commands
// are killed. Cancelling a finished run does nothing.
func (s *CommandService) Cancel(runID string) error {
	s.mu.Lock()
	r := s.runs[runID]
	s.mu.Unlock()
	if r != nil {
		r.cancel()
	}
	return nil
}

// Active lists runs in progress (for a page opened mid-run).
func (s *CommandService) Active() []RunInfo {
	s.mu.Lock()
	list := make([]*run, 0, len(s.runs))
	for _, r := range s.runs {
		list = append(list, r)
	}
	s.mu.Unlock()
	out := make([]RunInfo, 0, len(list))
	for _, r := range list {
		out = append(out, r.snapshot())
	}
	return out
}

// ---- history ----

func (s *CommandService) persist(info RunInfo) error {
	stored := info
	stored.Results = make([]ServerResult, len(info.Results))
	for i, r := range info.Results {
		var t1, t2 bool
		r.Stdout, t1 = truncateUTF8(r.Stdout, storedOutputMax)
		r.Stderr, t2 = truncateUTF8(r.Stderr, storedOutputMax)
		r.Truncated = r.Truncated || t1 || t2
		stored.Results[i] = r
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	title := info.Command
	if info.Spec.Kind == "preset" {
		title = info.Spec.Preset
	}
	title, _ = truncateUTF8(title, 200)
	_, err = s.core.DB.Exec(`INSERT INTO runs(id, kind, server, ref, started, finished, status, actor, version, data, log)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET finished=excluded.finished, status=excluded.status, data=excluded.data`,
		info.ID, runKind, "", title, info.Started, info.Finished, info.Status, info.Actor, "", string(data), "")
	return err
}

// History lists past runs, newest first (limit 1–500, default 50).
func (s *CommandService) History(limit int) ([]HistoryItem, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.core.DB.Query(`SELECT id, ref, started, finished, status, actor,
		COALESCE(json_extract(data, '$.summary'), '{}'), COALESCE(json_extract(data, '$.spec.kind'), ''),
		COALESCE(json_extract(data, '$.spec.preset'), ''), COALESCE(json_extract(data, '$.spec.snippet'), '')
		FROM runs WHERE kind=? ORDER BY started DESC LIMIT ?`, runKind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryItem{}
	for rows.Next() {
		var h HistoryItem
		var sum string
		if err := rows.Scan(&h.ID, &h.Title, &h.Started, &h.Finished, &h.Status, &h.Actor, &sum, &h.Kind, &h.Preset, &h.Snippet); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(sum), &h.Summary)
		// A live run's stored summary is from its start; use the live one.
		s.mu.Lock()
		r := s.runs[h.ID]
		s.mu.Unlock()
		if r != nil {
			h.Summary = r.snapshot().Summary
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetRun returns a run (live state while running, else from history).
func (s *CommandService) GetRun(id string) (RunInfo, error) {
	s.mu.Lock()
	r := s.runs[id]
	s.mu.Unlock()
	if r != nil {
		return r.snapshot(), nil
	}
	var data, status string
	var finished int64
	err := s.core.DB.QueryRow(`SELECT data, status, finished FROM runs WHERE id=? AND kind=?`, id, runKind).Scan(&data, &status, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return RunInfo{}, apperr.New("cmd.runNotFound")
	}
	if err != nil {
		return RunInfo{}, err
	}
	var info RunInfo
	if err := json.Unmarshal([]byte(data), &info); err != nil {
		return RunInfo{}, apperr.Wrap(err, "cmd.runNotFound")
	}
	info.Status = status
	info.Finished = finished
	if info.Status == "interrupted" {
		for i := range info.Results {
			switch info.Results[i].State {
			case StateQueued, StateConnecting, StateRunning:
				info.Results[i].State = StateCancelled
				info.Results[i].Error = apperr.New("cmd.interrupted")
			}
		}
	}
	info.Summary = summarize(info.Results)
	if info.Dangers == nil {
		info.Dangers = []Danger{}
	}
	return info, nil
}

// Rerun starts a new run with the same spec, on every server or only on the
// ones that did not succeed. Servers deleted since are left out.
func (s *CommandService) Rerun(runID string, onlyFailed bool) (string, error) {
	info, err := s.GetRun(runID)
	if err != nil {
		return "", err
	}
	spec := info.Spec
	spec.Servers = []string{}
	for _, r := range info.Results {
		if onlyFailed && r.State == StateDone {
			continue
		}
		if _, err := s.core.Store.Get(r.ServerID); err != nil {
			continue
		}
		spec.Servers = append(spec.Servers, r.ServerID)
	}
	if len(spec.Servers) == 0 {
		return "", apperr.New("cmd.nothingToRerun")
	}
	return s.Run(spec)
}

// PreviewRerun resolves what Rerun would do (for the confirmation dialog).
func (s *CommandService) PreviewRerun(runID string, onlyFailed bool) (Preview, error) {
	info, err := s.GetRun(runID)
	if err != nil {
		return Preview{}, err
	}
	spec := info.Spec
	spec.Servers = []string{}
	for _, r := range info.Results {
		if onlyFailed && r.State == StateDone {
			continue
		}
		if _, err := s.core.Store.Get(r.ServerID); err == nil {
			spec.Servers = append(spec.Servers, r.ServerID)
		}
	}
	if len(spec.Servers) == 0 {
		return Preview{}, apperr.New("cmd.nothingToRerun")
	}
	return s.Preview(spec)
}

// DeleteHistory removes finished runs (all when id is "").
func (s *CommandService) DeleteHistory(id string) error {
	var err error
	if id == "" {
		_, err = s.core.DB.Exec(`DELETE FROM runs WHERE kind=? AND status != 'running'`, runKind)
	} else {
		_, err = s.core.DB.Exec(`DELETE FROM runs WHERE kind=? AND id=? AND status != 'running'`, runKind, id)
	}
	s.core.Audit("", "cmd.history.delete", id, "", err)
	return err
}

// ---- background connections ----

// connRef counts in-flight uses of a background connection this service
// opened, so it can be closed after it has been idle for a while.
type connRef struct {
	n     int
	owned bool
	timer *time.Timer
}

func (s *CommandService) acquireConn(ctx context.Context, id string) (*sshx.Conn, error) {
	opened := true
	if c, err := s.core.Manager.Get(id); err == nil && c.Connected() {
		opened = false
	} else if c, err := s.core.Bg.Get(id); err == nil && c.Connected() {
		opened = false
	}
	conn, err := s.core.AnyConn(ctx, id)
	if err != nil {
		return nil, err
	}
	ui, _ := s.core.Manager.Get(id)
	s.connMu.Lock()
	defer s.connMu.Unlock()
	ref := s.conns[id]
	if ref == nil {
		ref = &connRef{}
		s.conns[id] = ref
	}
	if ref.timer != nil {
		ref.timer.Stop()
		ref.timer = nil
	}
	ref.n++
	if opened && conn != ui {
		ref.owned = true
	}
	return conn, nil
}

func (s *CommandService) releaseConn(id string) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	ref := s.conns[id]
	if ref == nil {
		return
	}
	ref.n--
	if ref.n > 0 {
		return
	}
	if !ref.owned {
		delete(s.conns, id)
		return
	}
	ref.timer = time.AfterFunc(bgIdleClose, func() {
		s.connMu.Lock()
		cur := s.conns[id]
		if cur != ref || ref.n > 0 {
			s.connMu.Unlock()
			return
		}
		delete(s.conns, id)
		s.connMu.Unlock()
		// Closing idle background connections is done centrally by core
		// (it knows whether another module is still using them).
	})
}
