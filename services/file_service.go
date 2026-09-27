package services

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"

	"server-manager/internal/apperr"
	"server-manager/internal/sshx"
)

const maxEditableSize = 10 << 20

// FileService exposes remote file-system operations over SFTP, with a few
// operations (search, recursive delete, archives) done through standard
// POSIX tools via exec for speed.
type FileService struct {
	core *Core

	idMu   sync.Mutex
	owners map[string]*ownerCache
}

func NewFileService(core *Core) *FileService {
	return &FileService{core: core, owners: map[string]*ownerCache{}}
}

type FileEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	IsDir      bool   `json:"isDir"`
	IsLink     bool   `json:"isLink"`
	LinkTarget string `json:"linkTarget"`
	Size       int64  `json:"size"`
	Mode       string `json:"mode"`
	Perm       uint32 `json:"perm"`
	ModTime    int64  `json:"modTime"`
	Owner      string `json:"owner"`
	Group      string `json:"group"`
}

type FileContent struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"modTime"`
	Binary   bool   `json:"binary"`
	TooLarge bool   `json:"tooLarge"`
	ReadOnly bool   `json:"readOnly"`
	Sudo     bool   `json:"sudo"`
}

type SaveResult struct {
	Conflict bool      `json:"conflict"`
	Entry    FileEntry `json:"entry"`
}

func (s *FileService) Home(connID string) (string, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return "", err
	}
	return conn.Home(), nil
}

func (s *FileService) ListDir(connID, dir string) ([]FileEntry, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, err
	}
	dir = cleanRemote(dir)
	var out []FileEntry
	err = conn.SFTP(func(c *sftp.Client) error {
		infos, err := c.ReadDir(dir)
		if err != nil {
			return err
		}
		owners := s.ownerNames(connID, c)
		out = make([]FileEntry, 0, len(infos))
		for _, fi := range infos {
			e := toEntry(path.Join(dir, fi.Name()), fi, owners)
			if e.IsLink {
				if target, err := c.ReadLink(e.Path); err == nil {
					e.LinkTarget = target
				}
				if st, err := c.Stat(e.Path); err == nil {
					e.IsDir = st.IsDir()
				}
			}
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		return nil, wrapFsErr(err, dir)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (s *FileService) Stat(connID, p string) (FileEntry, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return FileEntry{}, err
	}
	p = cleanRemote(p)
	var e FileEntry
	err = conn.SFTP(func(c *sftp.Client) error {
		fi, err := c.Stat(p)
		if err != nil {
			return err
		}
		e = toEntry(p, fi, s.ownerNames(connID, c))
		return nil
	})
	return e, wrapFsErr(err, p)
}

func (s *FileService) ReadFile(connID, p string) (FileContent, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return FileContent{}, err
	}
	p = cleanRemote(p)
	var fc FileContent
	err = conn.SFTP(func(c *sftp.Client) error {
		fi, err := c.Stat(p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return apperr.New("fs.isDir", "path", p)
		}
		fc = FileContent{Path: p, Size: fi.Size(), ModTime: fi.ModTime().Unix()}
		if fi.Size() > maxEditableSize {
			fc.TooLarge = true
			return nil
		}
		f, err := c.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, maxEditableSize+1))
		if err != nil {
			return err
		}
		fillContent(&fc, data)
		// Detect whether we could write it back without sudo.
		if wf, err := c.OpenFile(p, os.O_WRONLY); err == nil {
			wf.Close()
		} else {
			fc.ReadOnly = true
		}
		return nil
	})
	return fc, wrapFsErr(err, p)
}

// ReadFileSudo reads a file the login user cannot read, via `sudo cat`.
func (s *FileService) readFileSudo(connID, p, password string) (FileContent, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return FileContent{}, err
	}
	p = cleanRemote(p)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pw := s.core.SudoPassword(conn, password)
	st, err := sudoRun(ctx, conn, pw, "stat -c '%s %Y' -- "+sshx.ShellQuote(p), "")
	if err != nil {
		return FileContent{}, err
	}
	fc := FileContent{Path: p, Sudo: true}
	if parts := strings.Fields(st.Stdout); len(parts) == 2 {
		fc.Size, _ = strconv.ParseInt(parts[0], 10, 64)
		fc.ModTime, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	if fc.Size > maxEditableSize {
		fc.TooLarge = true
		return fc, nil
	}
	res, err := sudoRun(ctx, conn, pw, "cat -- "+sshx.ShellQuote(p), "")
	if err != nil {
		return FileContent{}, err
	}
	fillContent(&fc, []byte(res.Stdout))
	return fc, nil
}

func fillContent(fc *FileContent, data []byte) {
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 || !utf8.Valid(data) {
		fc.Binary = true
		return
	}
	fc.Content = string(data)
}

// WriteFile saves content. If expectedModTime is non-zero and the file changed
// on the server since it was opened, it returns Conflict instead of
// overwriting (unless force is set).
//
// The new content is first written to a temp file next to the target (so a
// failed upload never truncates the original), then copied into the target
// in place. Writing in place keeps the inode, owner, permissions and
// hard links intact, which matters for bind-mounted config files.
func (s *FileService) writeFile(connID, p, content string, expectedModTime int64, force bool) (SaveResult, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return SaveResult{}, err
	}
	p = cleanRemote(p)
	var res SaveResult
	err = conn.SFTP(func(c *sftp.Client) error {
		target, err := resolveLinks(c, p)
		if err != nil {
			return err
		}
		fi, statErr := c.Stat(target)
		if statErr == nil {
			if fi.IsDir() {
				return apperr.New("fs.isDir", "path", p)
			}
			if !force && expectedModTime != 0 && fi.ModTime().Unix() != expectedModTime {
				res.Conflict = true
				res.Entry = toEntry(p, fi, s.ownerNames(connID, c))
				return nil
			}
		}

		data := []byte(content)
		tmp := path.Join(path.Dir(target), "."+path.Base(target)+".sm-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		staged := false
		if tf, err := c.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL); err == nil {
			_, werr := tf.Write(data)
			cerr := tf.Close()
			if werr != nil || cerr != nil {
				_ = c.Remove(tmp)
				return firstErr(werr, cerr)
			}
			staged = true
		}

		if statErr != nil {
			// New file: move the staged copy into place, or write directly.
			if staged {
				if err := c.PosixRename(tmp, target); err == nil {
					_ = c.Chmod(target, 0o644)
					return s.statInto(connID, c, p, &res)
				}
				_ = c.Remove(tmp)
			}
		}

		f, err := c.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			if staged {
				_ = c.Remove(tmp)
			}
			return err
		}
		_, werr := f.Write(data)
		cerr := f.Close()
		if err := firstErr(werr, cerr); err != nil {
			if staged {
				return apperr.Wrap(err, "fs.writeFailedBackup", "tmp", tmp)
			}
			return err
		}
		if staged {
			_ = c.Remove(tmp)
		}
		return s.statInto(connID, c, p, &res)
	})
	return res, wrapFsErr(err, p)
}

// WriteFileSudo saves via `sudo sh -c 'cat > file'`, keeping inode and owner.
func (s *FileService) writeFileSudo(connID, p, content, password string, expectedModTime int64, force bool) (SaveResult, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return SaveResult{}, err
	}
	p = cleanRemote(p)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	pw := s.core.SudoPassword(conn, password)
	q := sshx.ShellQuote(p)
	if !force && expectedModTime != 0 {
		st, err := sudoRun(ctx, conn, pw, "stat -c '%Y' -- "+q+" 2>/dev/null || true", "")
		if err != nil {
			return SaveResult{}, err
		}
		if m, err := strconv.ParseInt(strings.TrimSpace(st.Stdout), 10, 64); err == nil && m != expectedModTime {
			return SaveResult{Conflict: true, Entry: FileEntry{Path: p, Name: path.Base(p), ModTime: m}}, nil
		}
	}
	if _, err := sudoRun(ctx, conn, pw, "cat > "+q, content); err != nil {
		return SaveResult{}, err
	}
	st, _ := sudoRun(ctx, conn, pw, "stat -c '%s %Y' -- "+q, "")
	e := FileEntry{Path: p, Name: path.Base(p)}
	if parts := strings.Fields(st.Stdout); len(parts) == 2 {
		e.Size, _ = strconv.ParseInt(parts[0], 10, 64)
		e.ModTime, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	return SaveResult{Entry: e}, nil
}

func (s *FileService) statInto(connID string, c *sftp.Client, p string, res *SaveResult) error {
	fi, err := c.Stat(p)
	if err != nil {
		return err
	}
	res.Entry = toEntry(p, fi, s.ownerNames(connID, c))
	return nil
}

func resolveLinks(c *sftp.Client, p string) (string, error) {
	for i := 0; i < 16; i++ {
		fi, err := c.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return p, nil
		}
		target, err := c.ReadLink(p)
		if err != nil {
			return "", err
		}
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(p), target)
		}
		p = target
	}
	return "", apperr.New("fs.tooManyLinks")
}

func (s *FileService) createFile(connID, p string) (FileEntry, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return FileEntry{}, err
	}
	p = cleanRemote(p)
	var e FileEntry
	err = conn.SFTP(func(c *sftp.Client) error {
		f, err := c.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
		if err != nil {
			if _, statErr := c.Lstat(p); statErr == nil {
				return apperr.New("fs.exists", "name", path.Base(p))
			}
			return err
		}
		f.Close()
		fi, err := c.Stat(p)
		if err != nil {
			return err
		}
		e = toEntry(p, fi, s.ownerNames(connID, c))
		return nil
	})
	return e, wrapFsErr(err, p)
}

func (s *FileService) createDir(connID, p string) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	p = cleanRemote(p)
	err = conn.SFTP(func(c *sftp.Client) error {
		if _, err := c.Lstat(p); err == nil {
			return apperr.New("fs.exists", "name", path.Base(p))
		}
		return c.MkdirAll(p)
	})
	return wrapFsErr(err, p)
}

func (s *FileService) rename(connID, from, to string) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	from, to = cleanRemote(from), cleanRemote(to)
	if from == to {
		return nil
	}
	err = conn.SFTP(func(c *sftp.Client) error {
		if _, err := c.Lstat(to); err == nil {
			return apperr.New("fs.exists", "name", path.Base(to))
		}
		return c.Rename(from, to)
	})
	return wrapFsErr(err, from)
}

// Move moves several paths into a directory.
func (s *FileService) move(connID string, paths []string, destDir string) error {
	destDir = cleanRemote(destDir)
	for _, p := range paths {
		p = cleanRemote(p)
		if destDir == p || strings.HasPrefix(destDir, p+"/") {
			return apperr.New("fs.moveIntoSelf", "path", p)
		}
		if err := s.rename(connID, p, path.Join(destDir, path.Base(p))); err != nil {
			return err
		}
	}
	return nil
}

// Copy duplicates paths into destDir using `cp -a`, falling back to an SFTP
// stream copy for single files when exec isn't allowed.
func (s *FileService) copy(connID string, paths []string, destDir string) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	destDir = cleanRemote(destDir)
	for _, p := range paths {
		p = cleanRemote(p)
		dst := uniqueName(conn, destDir, path.Base(p))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		res, err := conn.Exec(ctx, "cp -a -- "+sshx.ShellQuote(p)+" "+sshx.ShellQuote(dst), nil)
		cancel()
		if err == nil && res.ExitCode == 0 {
			continue
		}
		if err == nil && res.ExitCode != 0 && res.Stderr != "" && !strings.Contains(res.Stderr, "not found") {
			return fsCmdError(res, p)
		}
		err = conn.SFTP(func(c *sftp.Client) error {
			src, err := c.Open(p)
			if err != nil {
				return err
			}
			defer src.Close()
			out, err := c.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, src); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		})
		if err != nil {
			return wrapFsErr(err, p)
		}
	}
	return nil
}

func uniqueName(conn *sshx.Conn, dir, name string) string {
	candidate := path.Join(dir, name)
	_ = conn.SFTP(func(c *sftp.Client) error {
		ext := path.Ext(name)
		base := strings.TrimSuffix(name, ext)
		for i := 0; i < 1000; i++ {
			if _, err := c.Lstat(candidate); err != nil {
				return nil
			}
			suffix := " copy"
			if i > 0 {
				suffix = fmt.Sprintf(" copy %d", i+1)
			}
			candidate = path.Join(dir, base+suffix+ext)
		}
		return nil
	})
	return candidate
}

// Delete removes files and directories recursively.
func (s *FileService) delete(connID string, paths []string) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	for _, p := range paths {
		p = cleanRemote(p)
		if p == "/" || p == "." || p == "" {
			return apperr.New("fs.refuseRoot")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		res, err := conn.Exec(ctx, "rm -rf -- "+sshx.ShellQuote(p), nil)
		cancel()
		if err == nil && res.ExitCode == 0 {
			continue
		}
		if err == nil && strings.TrimSpace(res.Stderr) != "" {
			return fsCmdError(res, p)
		}
		// exec unavailable (e.g. sftp-only account): walk and delete via SFTP.
		if err := conn.SFTP(func(c *sftp.Client) error { return removeAll(c, p) }); err != nil {
			return wrapFsErr(err, p)
		}
	}
	return nil
}

func removeAll(c *sftp.Client, p string) error {
	fi, err := c.Lstat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !fi.IsDir() {
		return c.Remove(p)
	}
	entries, err := c.ReadDir(p)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeAll(c, path.Join(p, e.Name())); err != nil {
			return err
		}
	}
	return c.RemoveDirectory(p)
}

func (s *FileService) chmod(connID, p string, mode uint32, recursive bool) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	p = cleanRemote(p)
	if recursive {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, err := conn.Exec(ctx, fmt.Sprintf("chmod -R %o -- %s", mode&0o7777, sshx.ShellQuote(p)), nil)
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fsCmdError(res, p)
		}
		return nil
	}
	return wrapFsErr(conn.SFTP(func(c *sftp.Client) error {
		return c.Chmod(p, os.FileMode(mode&0o777)|unixSpecial(mode))
	}), p)
}

func unixSpecial(mode uint32) os.FileMode {
	var m os.FileMode
	if mode&0o4000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&0o2000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&0o1000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

type SearchHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Search finds files by name, or by content when content is true, using
// find/grep on the server. Results are capped at 500.
func (s *FileService) Search(connID, root, query string, content bool) ([]SearchHit, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	root = cleanRemote(root)
	const excludes = "node_modules .git .cache __pycache__ .venv"
	var cmd string
	if content {
		ex := ""
		for _, d := range strings.Fields(excludes) {
			ex += " --exclude-dir=" + d
		}
		q, r := sshx.ShellQuote(query), sshx.ShellQuote(root)
		gnu := "grep -rnIF -m 20" + ex + " -e " + q + " -- " + r + " 2>/dev/null"
		// BusyBox grep (Alpine, embedded) lacks -I/--exclude-dir: probe, then fall back.
		basic := "grep -rnF -e " + q + " -- " + r + " 2>/dev/null | grep -v -e /node_modules/ -e /.git/"
		cmd = "grep -I --exclude-dir=.git -q x /dev/null 2>/dev/null; if [ $? -eq 2 ]; then " + basic + "; else " + gnu + "; fi | head -n 500"
	} else {
		prune := []string{}
		for _, d := range strings.Fields(excludes) {
			prune = append(prune, "-name "+d)
		}
		cmd = "find " + sshx.ShellQuote(root) + " \\( " + strings.Join(prune, " -o ") + " \\) -prune -o -iname " +
			sshx.ShellQuote("*"+query+"*") + " -print 2>/dev/null | head -n 500"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := conn.Exec(ctx, cmd, nil)
	if err != nil {
		return nil, err
	}
	var hits []SearchHit
	sc := bufio.NewScanner(strings.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if !content {
			hits = append(hits, SearchHit{Path: line})
			continue
		}
		// path:line:text — paths may contain ':' so anchor on the root prefix.
		idx := strings.Index(line, ":")
		for idx >= 0 {
			rest := line[idx+1:]
			if j := strings.Index(rest, ":"); j > 0 {
				if n, err := strconv.Atoi(rest[:j]); err == nil {
					text := rest[j+1:]
					if len(text) > 300 {
						text = text[:300]
					}
					hits = append(hits, SearchHit{Path: line[:idx], Line: n, Text: text})
					break
				}
			}
			next := strings.Index(line[idx+1:], ":")
			if next < 0 {
				break
			}
			idx += next + 1
		}
	}
	return hits, nil
}

// Compress creates a tar.gz (or zip when name ends in .zip) of paths inside
// dir. Paths are relative to dir.
func (s *FileService) compress(connID, dir string, names []string, archiveName string) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return apperr.New("fs.noSelection")
	}
	dir = cleanRemote(dir)
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = sshx.ShellQuote(path.Base(n))
	}
	var cmd string
	if strings.HasSuffix(archiveName, ".zip") {
		cmd = "cd " + sshx.ShellQuote(dir) + " && zip -rq " + sshx.ShellQuote(archiveName) + " -- " + strings.Join(quoted, " ")
	} else {
		cmd = "cd " + sshx.ShellQuote(dir) + " && tar -czf " + sshx.ShellQuote(archiveName) + " -- " + strings.Join(quoted, " ")
	}
	return runChecked(conn, cmd, 30*time.Minute)
}

// Extract unpacks an archive into its own directory.
func (s *FileService) extract(connID, archive string) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	archive = cleanRemote(archive)
	dir := sshx.ShellQuote(path.Dir(archive))
	a := sshx.ShellQuote(archive)
	lower := strings.ToLower(archive)
	var cmd string
	switch {
	case strings.HasSuffix(lower, ".zip"):
		cmd = "unzip -o " + a + " -d " + dir
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		cmd = "tar -xzf " + a + " -C " + dir
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		cmd = "tar -xjf " + a + " -C " + dir
	case strings.HasSuffix(lower, ".tar.xz"), strings.HasSuffix(lower, ".txz"):
		cmd = "tar -xJf " + a + " -C " + dir
	case strings.HasSuffix(lower, ".tar"):
		cmd = "tar -xf " + a + " -C " + dir
	case strings.HasSuffix(lower, ".gz"):
		cmd = "gunzip -k " + a
	default:
		return apperr.New("fs.archiveUnsupported")
	}
	return runChecked(conn, cmd, 30*time.Minute)
}

func runChecked(conn *sshx.Conn, cmd string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := conn.Exec(ctx, cmd, nil)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return cmdError(res)
	}
	return nil
}

// ---- helpers ----

func cleanRemote(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	return path.Clean(p)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func wrapFsErr(err error, p string) error {
	if err == nil {
		return nil
	}
	var ae *apperr.Error
	switch {
	case errors.As(err, &ae):
		return err
	case errors.Is(err, fs.ErrPermission):
		return apperr.Wrap(err, "fs.permission", "path", p)
	case errors.Is(err, fs.ErrNotExist):
		return apperr.Wrap(err, "fs.notFound", "path", p)
	}
	return err
}

// fsCmdError maps a failed shell file operation to a coded error,
// recognising the common "Permission denied" case.
func fsCmdError(res sshx.ExecResult, p string) error {
	msg := strings.TrimSpace(res.Stderr)
	if strings.Contains(strings.ToLower(msg), "permission denied") || strings.Contains(msg, "Operation not permitted") {
		return apperr.New("fs.permission", "path", p).WithDetail(msg)
	}
	return cmdError(res)
}

func toEntry(p string, fi os.FileInfo, owners *ownerCache) FileEntry {
	e := FileEntry{
		Name:    fi.Name(),
		Path:    p,
		IsDir:   fi.IsDir(),
		IsLink:  fi.Mode()&fs.ModeSymlink != 0,
		Size:    fi.Size(),
		Mode:    fi.Mode().String(),
		Perm:    uint32(fi.Mode().Perm()),
		ModTime: fi.ModTime().Unix(),
	}
	if e.Name == "" || e.Name == "." {
		e.Name = path.Base(p)
	}
	if fi.Mode()&os.ModeSetuid != 0 {
		e.Perm |= 0o4000
	}
	if fi.Mode()&os.ModeSetgid != 0 {
		e.Perm |= 0o2000
	}
	if fi.Mode()&os.ModeSticky != 0 {
		e.Perm |= 0o1000
	}
	if st, ok := fi.Sys().(*sftp.FileStat); ok && owners != nil {
		e.Owner = owners.user(st.UID)
		e.Group = owners.group(st.GID)
	}
	return e
}

// ownerCache maps uid/gid to names by reading /etc/passwd and /etc/group once.
type ownerCache struct {
	users  map[uint32]string
	groups map[uint32]string
}

func (o *ownerCache) user(id uint32) string {
	if n, ok := o.users[id]; ok {
		return n
	}
	return strconv.FormatUint(uint64(id), 10)
}

func (o *ownerCache) group(id uint32) string {
	if n, ok := o.groups[id]; ok {
		return n
	}
	return strconv.FormatUint(uint64(id), 10)
}

func (s *FileService) ownerNames(connID string, c *sftp.Client) *ownerCache {
	s.idMu.Lock()
	defer s.idMu.Unlock()
	if oc, ok := s.owners[connID]; ok {
		return oc
	}
	oc := &ownerCache{users: parseIDFile(c, "/etc/passwd"), groups: parseIDFile(c, "/etc/group")}
	s.owners[connID] = oc
	return oc
}

func parseIDFile(c *sftp.Client, p string) map[uint32]string {
	out := map[uint32]string{}
	f, err := c.Open(p)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, 4<<20))
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) < 3 {
			continue
		}
		if id, err := strconv.ParseUint(parts[2], 10, 32); err == nil {
			out[uint32(id)] = parts[0]
		}
	}
	return out
}

// ListDirSudo lists a directory the login user cannot read, via sudo and
// POSIX stat (works with both GNU coreutils and BusyBox).
func (s *FileService) listDirSudo(connID, dir, password string) ([]FileEntry, error) {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return nil, err
	}
	dir = cleanRemote(dir)
	script := `cd -- "$1" || exit 1
for f in * .[!.]* ..?*; do
  if [ -e "$f" ] || [ -L "$f" ]; then
    if [ -d "$f" ]; then d=1; else d=0; fi
    printf '%s|' "$d"; stat -c '%F|%s|%a|%Y|%U|%G|%n' -- "$f"
  fi
done`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := sudoRun(ctx, conn, s.core.SudoPassword(conn, password), "sh -c "+sshx.ShellQuote(script)+" sh "+sshx.ShellQuote(dir), "")
	if err != nil {
		return nil, err
	}
	var out []FileEntry
	for _, line := range strings.Split(res.Stdout, "\n") {
		f := strings.SplitN(line, "|", 8)
		if len(f) != 8 {
			continue
		}
		size, _ := strconv.ParseInt(f[2], 10, 64)
		perm, _ := strconv.ParseUint(f[3], 8, 32)
		mtime, _ := strconv.ParseInt(f[4], 10, 64)
		mode := os.FileMode(perm & 0o777)
		isLink := strings.Contains(f[1], "link")
		switch {
		case isLink:
			mode |= os.ModeSymlink
		case f[1] == "directory":
			mode |= os.ModeDir
		}
		out = append(out, FileEntry{
			Name: f[7], Path: path.Join(dir, f[7]), IsDir: f[0] == "1", IsLink: isLink,
			Size: size, Mode: mode.String(), Perm: uint32(perm), ModTime: mtime, Owner: f[5], Group: f[6],
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// SudoOp performs a file operation as root. op is one of: mkdir, touch,
// rename (paths[0] -> dest), move/copy (paths into dest), delete, chmod.
func (s *FileService) sudoOp(connID, op string, paths []string, dest string, mode uint32, recursive bool, password string) error {
	conn, err := s.core.Conn(connID)
	if err != nil {
		return err
	}
	q := func(ps []string) string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = sshx.ShellQuote(cleanRemote(p))
		}
		return strings.Join(out, " ")
	}
	if len(paths) == 0 {
		return apperr.New("fs.noSelection")
	}
	var cmd string
	switch op {
	case "mkdir":
		cmd = "mkdir -p -- " + q(paths)
	case "touch":
		cmd = "set -C; : > " + q(paths[:1]) // noclobber: fail if it exists
	case "rename":
		cmd = "[ ! -e " + q([]string{dest}) + " ] && mv -- " + q(paths[:1]) + " " + q([]string{dest})
	case "move":
		cmd = "mv -- " + q(paths) + " " + q([]string{dest})
	case "copy":
		cmd = "cp -a -- " + q(paths) + " " + q([]string{dest})
	case "delete":
		for _, p := range paths {
			if cleanRemote(p) == "/" {
				return apperr.New("fs.refuseRoot")
			}
		}
		cmd = "rm -rf -- " + q(paths)
	case "chmod":
		flag := ""
		if recursive {
			flag = "-R "
		}
		cmd = fmt.Sprintf("chmod %s%o -- %s", flag, mode&0o7777, q(paths))
	default:
		return apperr.New("fs.invalidOp")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err = sudoRun(ctx, conn, s.core.SudoPassword(conn, password), cmd, "")
	return err
}
