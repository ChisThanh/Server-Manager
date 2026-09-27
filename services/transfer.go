package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/sftp"
	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
)

type transfer struct {
	ev       TransferEvent
	done     atomic.Int64
	cancel   context.CancelFunc
	ctx      context.Context
	lastEmit time.Time
	mu       sync.Mutex
}

var (
	transfersMu sync.Mutex
	transfers   = map[string]*transfer{}
)

func newTransfer(connID, kind, name, target string) *transfer {
	ctx, cancel := context.WithCancel(context.Background())
	t := &transfer{
		ev:     TransferEvent{ID: uuid.NewString(), ConnID: connID, Kind: kind, Name: name, Target: target, State: "running"},
		ctx:    ctx,
		cancel: cancel,
	}
	transfersMu.Lock()
	transfers[t.ev.ID] = t
	transfersMu.Unlock()
	return t
}

func (t *transfer) reset() {
	t.done.Store(0)
	t.mu.Lock()
	t.ev.FilesDone = 0
	t.mu.Unlock()
}

func (t *transfer) add(n int64) {
	t.done.Add(n)
	t.emit(false)
}

func (t *transfer) emit(force bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !force && time.Since(t.lastEmit) < 150*time.Millisecond {
		return
	}
	t.lastEmit = time.Now()
	ev := t.ev
	ev.Done = t.done.Load()
	emit(EventTransfer, ev)
}

func (t *transfer) finish(err error) {
	t.mu.Lock()
	switch {
	case err == nil:
		t.ev.State = "done"
	case errors.Is(err, context.Canceled):
		t.ev.State = "cancelled"
	default:
		t.ev.State = "error"
		t.ev.Error = apperr.From(wrapFsErr(err, t.ev.Current))
	}
	t.mu.Unlock()
	t.emit(true)
	t.cancel()
	transfersMu.Lock()
	delete(transfers, t.ev.ID)
	transfersMu.Unlock()
}

func (s *FileService) CancelTransfer(id string) {
	transfersMu.Lock()
	t, ok := transfers[id]
	transfersMu.Unlock()
	if ok {
		t.cancel()
	}
}

// progressReader counts bytes and aborts on cancellation. It exposes Size so
// sftp's ReadFrom can use concurrent writes.
type progressReader struct {
	r    io.Reader
	size int64
	t    *transfer
}

func (p *progressReader) Read(b []byte) (int, error) {
	if err := p.t.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.r.Read(b)
	p.t.add(int64(n))
	return n, err
}

func (p *progressReader) Size() int64 { return p.size }

type progressWriter struct {
	w io.Writer
	t *transfer
}

func (p *progressWriter) Write(b []byte) (int, error) {
	if err := p.t.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := p.w.Write(b)
	p.t.add(int64(n))
	return n, err
}

// PickAndUpload opens a native picker and uploads the chosen files (or a
// folder when folders is true) into remoteDir. Returns the transfer id, or
// "" if the user cancelled the dialog.
func (s *FileService) PickAndUpload(connID, remoteDir string, folders bool, title string) (string, error) {
	dlg := application.Get().Dialog.OpenFile().SetTitle(title).CanCreateDirectories(false)
	if folders {
		dlg.CanChooseDirectories(true).CanChooseFiles(false)
	} else {
		dlg.CanChooseFiles(true)
	}
	paths, err := dlg.PromptForMultipleSelection()
	if err != nil || len(paths) == 0 {
		return "", nil
	}
	return s.Upload(connID, paths, remoteDir)
}

// Upload copies local files/folders into remoteDir in the background and
// reports progress through transfer events.
func (s *FileService) upload(connID string, localPaths []string, remoteDir string) (string, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	if len(localPaths) == 0 {
		return "", apperr.New("fs.noFiles")
	}
	remoteDir = cleanRemote(remoteDir)

	type job struct {
		local, remote string
		dir           bool
		size          int64
	}
	var jobs []job
	var total int64
	for _, lp := range localPaths {
		base := filepath.Dir(lp)
		err := filepath.WalkDir(lp, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(base, p)
			rp := path.Join(remoteDir, filepath.ToSlash(rel))
			if d.IsDir() {
				jobs = append(jobs, job{local: p, remote: rp, dir: true})
				return nil
			}
			if !d.Type().IsRegular() {
				return nil // skip sockets, symlinks, devices
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			jobs = append(jobs, job{local: p, remote: rp, size: info.Size()})
			total += info.Size()
			return nil
		})
		if err != nil {
			return "", err
		}
	}

	t := newTransfer(connID, "upload", filepath.Base(localPaths[0]), remoteDir)
	t.ev.More = len(localPaths) - 1
	t.ev.Total = total
	for _, j := range jobs {
		if !j.dir {
			t.ev.Files++
		}
	}
	t.emit(true)

	go func() {
		err := conn.SFTP(func(c *sftp.Client) error {
			t.reset() // the closure is re-run once after a reconnect
			for _, j := range jobs {
				if err := t.ctx.Err(); err != nil {
					return err
				}
				if j.dir {
					if err := c.MkdirAll(j.remote); err != nil {
						return apperr.Wrap(err, "fs.mkdirFailed", "path", j.remote)
					}
					continue
				}
				t.mu.Lock()
				t.ev.Current = j.remote
				t.mu.Unlock()
				if err := uploadFile(c, t, j.local, j.remote, j.size); err != nil {
					return fmt.Errorf("%s: %w", filepath.Base(j.local), err)
				}
				t.mu.Lock()
				t.ev.FilesDone++
				t.mu.Unlock()
			}
			return nil
		})
		t.finish(err)
	}()
	return t.ev.ID, nil
}

func uploadFile(c *sftp.Client, t *transfer, local, remote string, size int64) error {
	in, err := os.Open(local)
	if err != nil {
		return err
	}
	defer in.Close()
	st, _ := in.Stat()
	out, err := c.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return err
	}
	_, err = out.ReadFrom(&progressReader{r: in, size: size, t: t})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			_ = c.Remove(remote)
		}
		return err
	}
	if st != nil && st.Mode().Perm()&0o111 != 0 {
		_ = c.Chmod(remote, st.Mode().Perm())
	}
	return nil
}

// PickAndDownload asks where to save and downloads a file or folder.
func (s *FileService) PickAndDownload(connID, remotePath string, isDir bool, title string) (string, error) {
	remotePath = cleanRemote(remotePath)
	var dest string
	var err error
	if isDir {
		dest, err = application.Get().Dialog.OpenFile().
			SetTitle(title).
			CanChooseDirectories(true).CanChooseFiles(false).CanCreateDirectories(true).
			PromptForSingleSelection()
		if err == nil && dest != "" {
			dest = filepath.Join(dest, path.Base(remotePath))
		}
	} else {
		dest, err = application.Get().Dialog.SaveFile().
			SetFilename(path.Base(remotePath)).
			CanCreateDirectories(true).
			PromptForSingleSelection()
	}
	if err != nil || dest == "" {
		return "", nil
	}
	return s.Download(connID, remotePath, dest)
}

// Download copies a remote file or directory to localPath.
func (s *FileService) download(connID, remotePath, localPath string) (string, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	remotePath = cleanRemote(remotePath)
	t := newTransfer(connID, "download", path.Base(remotePath), localPath)
	t.emit(true)

	go func() {
		err := conn.SFTP(func(c *sftp.Client) error {
			t.reset()
			t.mu.Lock()
			t.ev.Total, t.ev.Files = 0, 0
			t.mu.Unlock()
			fi, err := c.Stat(remotePath)
			if err != nil {
				return err
			}
			if !fi.IsDir() {
				t.mu.Lock()
				t.ev.Total, t.ev.Files = fi.Size(), 1
				t.mu.Unlock()
				return downloadFile(c, t, remotePath, localPath)
			}
			type job struct {
				remote, local string
				dir           bool
			}
			var jobs []job
			w := c.Walk(remotePath)
			for w.Step() {
				if err := w.Err(); err != nil {
					continue // unreadable entries are skipped
				}
				rel, _ := filepath.Rel(remotePath, w.Path())
				lp := filepath.Join(localPath, filepath.FromSlash(rel))
				st := w.Stat()
				if st.IsDir() {
					jobs = append(jobs, job{remote: w.Path(), local: lp, dir: true})
				} else if st.Mode().IsRegular() {
					jobs = append(jobs, job{remote: w.Path(), local: lp})
					t.mu.Lock()
					t.ev.Total += st.Size()
					t.ev.Files++
					t.mu.Unlock()
				}
			}
			t.emit(true)
			for _, j := range jobs {
				if err := t.ctx.Err(); err != nil {
					return err
				}
				if j.dir {
					if err := os.MkdirAll(j.local, 0o755); err != nil {
						return err
					}
					continue
				}
				t.mu.Lock()
				t.ev.Current = j.remote
				t.mu.Unlock()
				if err := downloadFile(c, t, j.remote, j.local); err != nil {
					return fmt.Errorf("%s: %w", j.remote, err)
				}
			}
			return nil
		})
		t.finish(err)
	}()
	return t.ev.ID, nil
}

func downloadFile(c *sftp.Client, t *transfer, remote, local string) error {
	in, err := c.Open(remote)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	tmp := local + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = in.WriteTo(&progressWriter{w: out, t: t})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	t.mu.Lock()
	t.ev.FilesDone++
	t.mu.Unlock()
	return os.Rename(tmp, local)
}
