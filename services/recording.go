package services

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"server-manager/internal/apperr"
)

// recorder writes a terminal session as an asciicast v2 file. Only output
// is recorded (like `script`): the shell echoes typed commands, while
// passwords typed at no-echo prompts never reach the file.
type recorder struct {
	mu    sync.Mutex
	f     *os.File
	w     *bufio.Writer
	start time.Time
	carry []byte // incomplete UTF-8 sequence from the previous chunk
}

func recordingsDir(base string) string { return filepath.Join(base, "recordings") }

func newRecorder(base, serverID, termID, title string, cols, rows int) (*recorder, string, error) {
	dir := filepath.Join(recordingsDir(base), serverID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	now := time.Now()
	name := now.Format("20060102-150405") + "-" + termID[:8] + ".cast"
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return nil, "", err
	}
	r := &recorder{f: f, w: bufio.NewWriterSize(f, 32*1024), start: now}
	hdr, _ := json.Marshal(map[string]any{"version": 2, "width": cols, "height": rows, "timestamp": now.Unix(), "title": title,
		"env": map[string]string{"TERM": "xterm-256color"}})
	r.w.Write(hdr)
	r.w.WriteByte('\n')
	return r, serverID + "/" + name, nil
}

func (r *recorder) output(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	data := append(r.carry, p...)
	// Keep a trailing partial rune for the next chunk.
	cut := len(data)
	for i := len(data) - 1; i >= 0 && i >= len(data)-3; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	r.carry = append([]byte(nil), data[cut:]...)
	r.event("o", string(data[:cut]))
}

func (r *recorder) resize(cols, rows int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		r.event("r", itoa(cols)+"x"+itoa(rows))
	}
}

func (r *recorder) event(kind, data string) {
	if data == "" {
		return
	}
	line, _ := json.Marshal([]any{float64(time.Since(r.start).Microseconds()) / 1e6, kind, data})
	r.w.Write(line)
	r.w.WriteByte('\n')
	if r.w.Buffered() > 16*1024 {
		r.w.Flush()
	}
}

func (r *recorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return
	}
	r.w.Flush()
	r.f.Close()
	r.f = nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// Recording describes a saved terminal session.
type Recording struct {
	File    string `json:"file"` // "<serverId>/<name>.cast"
	Server  string `json:"server"`
	Started int64  `json:"started"` // unix ms
	Size    int64  `json:"size"`
}

var castName = regexp.MustCompile(`^[A-Za-z0-9-]+/[0-9]{8}-[0-9]{6}-[0-9a-f-]{8}\.cast$`)

// Recordings lists saved terminal sessions (all servers when serverID is "").
func (s *TerminalService) Recordings(serverID string) ([]Recording, error) {
	root := recordingsDir(s.core.Dir)
	out := []Recording{}
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	for _, d := range dirs {
		if !d.IsDir() || (serverID != "" && d.Name() != serverID) {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, d.Name()))
		for _, f := range files {
			rel := d.Name() + "/" + f.Name()
			if !castName.MatchString(rel) {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			ts, _ := time.ParseInLocation("20060102-150405", f.Name()[:15], time.Local)
			out = append(out, Recording{File: rel, Server: d.Name(), Started: ts.UnixMilli(), Size: info.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started > out[j].Started })
	return out, nil
}

func (s *TerminalService) castPath(file string) (string, error) {
	if !castName.MatchString(file) || strings.Contains(file, "..") {
		return "", apperr.New("term.recordingNotFound")
	}
	return filepath.Join(recordingsDir(s.core.Dir), filepath.FromSlash(file)), nil
}

// ReadRecording returns an asciicast file's content (max 50 MB).
func (s *TerminalService) ReadRecording(file string) (string, error) {
	p, err := s.castPath(file)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", apperr.New("term.recordingNotFound")
	}
	if info.Size() > 50<<20 {
		return "", apperr.New("term.recordingTooLarge")
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

func (s *TerminalService) DeleteRecording(file string) error {
	p, err := s.castPath(file)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	s.core.Audit("", "terminal.recordingDelete", file, "", err)
	return err
}
