package logs

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

const (
	maxStreamsPerServer = 4
	streamFlushEvery    = 100 * time.Millisecond // at most ~10 events/s
	streamBatchMax      = 2000                   // lines kept between flushes
)

type stream struct {
	id     string
	connID string
	cancel context.CancelFunc
	stdin  *io.PipeWriter
	done   chan struct{}

	mu      sync.Mutex
	pending []LogLine
	dropped int
}

// followWrap runs a follow command so that it dies when the SSH channel's
// stdin closes (stream stopped, connection lost), even under sudo where a
// signal cannot reach it.
func followWrap(cmd string) string {
	// Background lists get /dev/null as stdin, so the watcher reads the
	// channel's stdin through fd 3.
	return `exec 3<&0
` + cmd + ` </dev/null 3<&- &
p=$!
( cat <&3 >/dev/null 2>&1; kill $p 2>/dev/null ) >/dev/null 2>&1 &
w=$!
exec 3<&-
wait $p
s=$?
kill $w 2>/dev/null
exit $s`
}

// StartStream follows a log source live. Lines arrive as "logs:lines"
// events carrying the returned stream id; the final event has done=true.
// The time range is ignored; Lines is the initial backlog (max 1000).
func (s *LogsService) StartStream(connID string, spec QuerySpec, sudoPassword string) (string, error) {
	sp, err := normalize(spec, false)
	if err != nil {
		return "", err
	}
	if sp.Kind == "file" && len(sp.Target) > 3 && sp.Target[len(sp.Target)-3:] == ".gz" {
		return "", apperr.New("logs.cannotFollow")
	}
	conn, err := s.conn(connID)
	if err != nil {
		return "", err
	}
	s.streamsMu.Lock()
	n := 0
	for _, st := range s.streams {
		if st.connID == connID {
			n++
		}
	}
	s.streamsMu.Unlock()
	if n >= maxStreamsPerServer {
		return "", apperr.New("logs.tooManyStreams", "max", strconv.Itoa(maxStreamsPerServer))
	}

	pctx, pcancel := core.Timeout(30 * time.Second)
	defer pcancel()
	c, err := s.getCaps(pctx, conn)
	if err != nil {
		return "", err
	}
	sudo, err := s.needSudo(pctx, conn, c, sp)
	if err != nil {
		return "", err
	}
	p := build(sp, c, modeFollow)
	pr, pw := io.Pipe()
	var extra io.Reader = pr
	if p.input != "" {
		extra = io.MultiReader(strings.NewReader(p.input), pr)
	}
	full, stdin, err := s.prepare(pctx, conn, followWrap(p.cmd), sudo, sudoPassword, extra)
	if err != nil {
		pw.Close()
		return "", err
	}
	s.auditSudoRead(connID, sp, sudo)

	ctx, cancel := context.WithCancel(context.Background())
	st := &stream{id: uuid.NewString(), connID: connID, cancel: cancel, stdin: pw, done: make(chan struct{})}
	s.streamsMu.Lock()
	n = 0
	for _, o := range s.streams {
		if o.connID == connID {
			n++
		}
	}
	if n >= maxStreamsPerServer {
		s.streamsMu.Unlock()
		cancel()
		pw.Close()
		return "", apperr.New("logs.tooManyStreams", "max", strconv.Itoa(maxStreamsPerServer))
	}
	s.streams[st.id] = st
	s.streamsMu.Unlock()

	m, _ := newMatcher(sp.Search, sp.Regex, sp.CaseSensitive)
	var lvl *regexp.Regexp
	if !isJournal(sp.Kind) {
		if lp := levelPattern(sp.Level); lp != "" {
			lvl = regexp.MustCompile("(?i)" + lp)
		}
	}
	tp := &textParser{loc: c.loc, now: time.Now(), syslog: p.syslog}
	onLine := func(b []byte) bool {
		if len(bytes.TrimSpace(b)) == 0 {
			return true
		}
		var l LogLine
		if p.json {
			var ok bool
			if l, ok = parseJournal(b, c.loc); !ok || !m.match(l.Message) {
				return true
			}
		} else {
			l = tp.parse(string(b))
			if !m.match(l.Raw) || (lvl != nil && !lvl.MatchString(l.Raw)) {
				return true
			}
		}
		st.push(l)
		return true
	}

	go s.watchConn(ctx, st, conn)
	go st.flushLoop(ctx)
	go func() {
		defer close(st.done)
		lw := &lineWriter{fn: onLine}
		var eb tailBuf
		code, err := conn.Stream(ctx, full, stdin, lw, &eb)
		lw.flush()
		var e *apperr.Error
		switch {
		case ctx.Err() != nil:
			// stopped
		case err != nil:
			e = apperr.From(err)
		default:
			if me := markerError(string(eb.b)); me != nil {
				e = apperr.From(me)
				if e.Code == "logs.fileNotFound" {
					e.Params = map[string]string{"path": sp.Target}
				}
			} else if code != 0 && code != 143 && code != -1 {
				e = apperr.From(apperr.New("logs.streamEnded", "code", strconv.Itoa(code)).WithDetail(string(eb.b)))
			}
		}
		cancel()
		pw.Close()
		s.streamsMu.Lock()
		delete(s.streams, st.id)
		s.streamsMu.Unlock()
		st.mu.Lock()
		lines, dropped := st.pending, st.dropped
		st.pending, st.dropped = nil, 0
		st.mu.Unlock()
		if lines == nil {
			lines = []LogLine{}
		}
		core.Emit(EventLines, LinesEvent{StreamID: st.id, Lines: lines, Dropped: dropped, Done: true, Error: e})
	}()
	return st.id, nil
}

func (st *stream) push(l LogLine) {
	st.mu.Lock()
	st.pending = append(st.pending, l)
	if over := len(st.pending) - streamBatchMax; over > 0 {
		st.pending = st.pending[over:]
		st.dropped += over
	}
	st.mu.Unlock()
}

func (st *stream) flushLoop(ctx context.Context) {
	t := time.NewTicker(streamFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st.mu.Lock()
		lines, dropped := st.pending, st.dropped
		st.pending, st.dropped = nil, 0
		st.mu.Unlock()
		if len(lines) > 0 || dropped > 0 {
			if lines == nil {
				lines = []LogLine{}
			}
			core.Emit(EventLines, LinesEvent{StreamID: st.id, Lines: lines, Dropped: dropped})
		}
	}
}

// watchConn stops the stream when its connection is closed or replaced.
func (s *LogsService) watchConn(ctx context.Context, st *stream, conn *sshx.Conn) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur, err := s.core.Manager.Get(st.connID)
		if err != nil || cur != conn || !conn.Connected() {
			st.stop()
			return
		}
	}
}

func (st *stream) stop() {
	st.stdin.Close()
	st.cancel()
}

// StopStream stops a live stream (no error if it already ended).
func (s *LogsService) StopStream(streamID string) error {
	s.streamsMu.Lock()
	st := s.streams[streamID]
	s.streamsMu.Unlock()
	if st == nil {
		return nil
	}
	st.stop()
	select {
	case <-st.done:
	case <-time.After(6 * time.Second):
	}
	return nil
}

// StopAllStreams stops every stream of a server (e.g. when the panel closes).
func (s *LogsService) StopAllStreams(connID string) {
	s.streamsMu.Lock()
	var list []*stream
	for _, st := range s.streams {
		if connID == "" || st.connID == connID {
			list = append(list, st)
		}
	}
	s.streamsMu.Unlock()
	for _, st := range list {
		st.stop()
	}
	for _, st := range list {
		select {
		case <-st.done:
		case <-time.After(6 * time.Second):
		}
	}
}

func (s *LogsService) stopAll() { s.StopAllStreams("") }
