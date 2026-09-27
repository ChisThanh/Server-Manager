package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// RestoreOptions describes where and how to restore a backup.
type RestoreOptions struct {
	JobID string `json:"jobId"`
	Key   string `json:"key"`
	// Target: files → staging | original; redis → staging | replace;
	// custom → staging | command. Ignored for databases and volumes.
	Target string `json:"target"`
	// Dir is the staging directory (files/redis/custom staging).
	Dir string `json:"dir"`
	// Database is the target database (postgres custom dumps, mysql single db).
	Database string `json:"database"`
	CreateDB bool   `json:"createDb"`
	// Volume is the target Docker volume (created if missing).
	Volume         string `json:"volume"`
	ClearVolume    bool   `json:"clearVolume"`
	StopContainers bool   `json:"stopContainers"`
	// Passphrase overrides the stored one (backups made before a change).
	Passphrase string `json:"passphrase"`
	// TempDir is where the verified payload is staged on the server.
	TempDir string `json:"tempDir"`
}

func validateRestore(m *Manifest, j *Job, o *RestoreOptions) error {
	if o.TempDir == "" {
		o.TempDir = "/var/tmp"
	}
	if !validServerPath(o.TempDir) {
		return invalid("tempDir")
	}
	needDir := func() error {
		o.Dir = strings.TrimRight(strings.TrimSpace(o.Dir), "/")
		if !validServerPath(o.Dir) || o.Dir == "/" {
			return invalid("dir")
		}
		return nil
	}
	switch m.Type {
	case TypeFiles:
		if o.Target != "original" {
			o.Target = "staging"
			return needDir()
		}
	case TypePostgres:
		if m.DumpFormat == "pg-custom" {
			if o.Database == "" {
				o.Database = m.Database
			}
			if !reDBName.MatchString(o.Database) {
				return invalid("database")
			}
		}
	case TypeMySQL:
		if m.Database != "" {
			if o.Database == "" {
				o.Database = m.Database
			}
			if !reDBName.MatchString(o.Database) {
				return invalid("database")
			}
		}
	case TypeVolume:
		if o.Volume == "" {
			o.Volume = m.Volume
		}
		if !reVolume.MatchString(o.Volume) {
			return invalid("volume")
		}
	case TypeRedis:
		if o.Target != "replace" {
			o.Target = "staging"
			return needDir()
		}
	case TypeCustom:
		if o.Target == "command" {
			if j.RestoreCommand == "" {
				return invalid("restoreCommand")
			}
			return nil
		}
		o.Target = "staging"
		return needDir()
	default:
		return invalid("type")
	}
	return nil
}

// restoreRoot reports whether the restore command must run as root.
func restoreRoot(j *Job, m *Manifest, o *RestoreOptions) bool {
	switch m.Type {
	case TypeFiles:
		return o.Target == "original" || j.Sudo
	case TypeRedis:
		return o.Target == "replace" || j.Sudo
	case TypeCustom:
		return j.Sudo
	}
	return j.runsAsRoot()
}

// readManifest loads and checks the manifest of a backup object.
func readManifest(ctx context.Context, sk sink, key, name string) (*Manifest, error) {
	b, err := sk.GetSmall(ctx, key+manifestSuffix, 1<<20)
	if err != nil {
		if apperr.HasCode(err, "backup.objectNotFound") {
			return nil, apperr.New("backup.noManifest")
		}
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil || m.Format < 1 {
		return nil, apperr.New("backup.badManifest")
	}
	if m.Object != name || len(m.SHA256) != 64 {
		return nil, apperr.New("backup.badManifest")
	}
	return &m, nil
}

// errReader remembers the first non-EOF read error.
type errReader struct {
	r   io.Reader
	mu  sync.Mutex
	err error
}

func (e *errReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF {
		e.mu.Lock()
		if e.err == nil {
			e.err = err
		}
		e.mu.Unlock()
	}
	return n, err
}

func (e *errReader) Err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

func checksumErr(what string) error {
	return apperr.New("backup.checksumMismatch", "what", what)
}

// restore applies a backup. The payload is verified against the manifest
// (sha256) BEFORE anything on the server is changed.
func (s *BackupService) restore(ctx context.Context, j *core.Job, job Job, o RestoreOptions, pw string) (m *Manifest, err error) {
	dest, err := s.getDest(job.Destination)
	if err != nil {
		return nil, err
	}
	name, err := checkKey(&job, o.Key)
	if err != nil {
		return nil, err
	}
	conn, err := s.core.AnyConn(ctx, job.Server)
	if err != nil {
		return nil, err
	}
	// Reading the stored backup uses the job's privileges.
	sk, err := s.sinkFor(dest, conn, job.runsAsRoot(), pw)
	if err != nil {
		return nil, err
	}
	j.Step("Reading the manifest of %s", name)
	m, err = readManifest(ctx, sk, o.Key, name)
	if err != nil {
		return nil, err
	}
	if m.JobID != job.ID {
		j.Logf("\x1b[33mnote: this backup was made by another job (%s)\x1b[0m", m.JobName)
	}
	if err := validateRestore(m, &job, &o); err != nil {
		return m, err
	}
	root := restoreRoot(&job, m, &o)
	passphrase := o.Passphrase
	if m.Encrypted && passphrase == "" {
		passphrase = store.Keychain(jobSecret(job.ID, "passphrase"))
	}
	var dbpw string
	if job.AuthMode == "password" {
		dbpw = store.Keychain(jobSecret(job.ID, "dbpass"))
	}

	var file string
	if dest.Type == DestServer && !m.Encrypted {
		p := path.Join(dest.Path, o.Key)
		j.Step("Verifying the checksum on the server")
		res, err := s.core.RunOK(ctx, conn, "sha256sum < "+core.Q(p)+" | cut -d ' ' -f 1", job.runsAsRoot(), pw, "")
		if err != nil {
			return m, err
		}
		if strings.TrimSpace(res.Stdout) != m.SHA256 {
			return m, checksumErr("payload")
		}
		j.Logf("sha256 OK (%s)", m.SHA256)
		file = core.Q(p)
	} else {
		tmp := path.Join(o.TempDir, "smrestore-"+uuid.NewString()[:8]+"-"+restoreFileName(m))
		defer func() {
			cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _ = s.core.Run(cctx, conn, "rm -f -- "+core.Q(tmp), false, "", "")
		}()
		j.Step("Downloading and verifying the backup (staged in %s)", tmp)
		if err := s.fetchToServer(ctx, j, sk, o.Key, m, passphrase, conn, tmp); err != nil {
			return m, err
		}
		file = core.Q(tmp)
	}

	if m.Type == TypeVolume && o.StopContainers {
		ids, err := s.volumeContainers(ctx, conn, o.Volume, pw)
		if err != nil {
			return m, err
		}
		if len(ids) > 0 {
			j.Step("Stopping %d container(s) using volume %s", len(ids), o.Volume)
			if _, err := s.core.RunOK(ctx, conn, "docker stop "+strings.Join(ids, " "), true, pw, ""); err != nil {
				s.startContainers(conn, ids, pw, j)
				return m, err
			}
			defer s.startContainers(conn, ids, pw, j)
		}
	}

	j.Step("Restoring")
	var stdin io.Reader
	if job.AuthMode == "password" {
		stdin = strings.NewReader(dbpw + "\n")
	}
	errTail := &tailBuffer{max: 4000}
	code, err := s.stream(ctx, conn, restoreScript(&job, m, &o, file), root, pw, stdin, j, io.MultiWriter(j, errTail))
	if err != nil {
		return m, err
	}
	if code != 0 {
		return m, apperr.New("backup.restoreFailed", "code", strconv.Itoa(code)).WithDetail(lastLines(errTail.String(), 6))
	}
	j.Logf("\x1b[32m✔ Restore finished\x1b[0m")
	return m, nil
}

// fetchToServer streams a backup from its destination through decryption
// into a private temp file on the server and verifies both checksums.
func (s *BackupService) fetchToServer(ctx context.Context, j *core.Job, sk sink, key string, m *Manifest, passphrase string, c *sshx.Conn, tmp string) error {
	r, err := sk.Open(ctx, key)
	if err != nil {
		return err
	}
	defer r.Close()
	objHC := newHashCounter()
	var payload io.Reader = io.TeeReader(r, objHC)
	if m.Encrypted {
		if payload, err = decryptReader(payload, passphrase); err != nil {
			return err
		}
	}
	payHC := newHashCounter()
	er := &errReader{r: io.TeeReader(payload, payHC)}
	errTail := &tailBuffer{max: 2000}
	code, err := s.stream(ctx, c, "umask 077; set -C; cat > "+core.Q(tmp), false, "", er, nil, errTail)
	if err != nil {
		return err
	}
	if rerr := er.Err(); rerr != nil {
		return apperr.Wrap(rerr, "backup.readFailed")
	}
	if code != 0 {
		return apperr.New("backup.stageFailed", "path", tmp).WithDetail(errTail.String())
	}
	if m.ObjectSHA256 != "" && (objHC.Sum() != m.ObjectSHA256 || objHC.n != m.ObjectSize) {
		return checksumErr("object")
	}
	if payHC.Sum() != m.SHA256 || payHC.n != m.Size {
		return checksumErr("payload")
	}
	j.Logf("sha256 OK (%s, %s)", m.SHA256, humanBytes(m.Size))
	return nil
}

// VerifyResult summarizes a verification.
type VerifyResult struct {
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Entries int    `json:"entries"` // tar entries (tar payloads)
	Format  string `json:"format"`
}

// verifyObject re-reads a stored backup, decrypts it and checks the hashes.
// deep also decompresses and checks the dump structure (tar entries,
// pg_dump/redis magic). dst, when given, receives the stored bytes (raw) or
// the payload (decrypt) — used by downloads.
func (s *BackupService) verifyObject(ctx context.Context, j *core.Job, sk sink, key string, m *Manifest, passphrase string, deep bool) (VerifyResult, error) {
	return s.readVerified(ctx, j, sk, key, m, passphrase, deep, nil, false)
}

func (s *BackupService) readVerified(ctx context.Context, j *core.Job, sk sink, key string, m *Manifest, passphrase string, deep bool, dst io.Writer, dstPayload bool) (VerifyResult, error) {
	var res VerifyResult
	r, err := sk.Open(ctx, key)
	if err != nil {
		return res, err
	}
	defer r.Close()
	objHC := newHashCounter()
	var raw io.Reader = io.TeeReader(ctxReader{r: r, done: ctx.Err}, objHC)
	if dst != nil && !dstPayload {
		raw = io.TeeReader(raw, dst)
	}
	payload := raw
	if m.Encrypted {
		if passphrase == "" {
			// Without the passphrase only the stored bytes can be checked.
			if _, err := io.Copy(io.Discard, raw); err != nil {
				return res, apperr.Wrap(err, "backup.readFailed")
			}
			if objHC.Sum() != m.ObjectSHA256 {
				return res, checksumErr("object")
			}
			return res, apperr.New("backup.noPassphrase")
		}
		if payload, err = decryptReader(raw, passphrase); err != nil {
			return res, err
		}
	}
	payHC := newHashCounter()
	var tee io.Reader = io.TeeReader(payload, payHC)
	if dst != nil && dstPayload {
		tee = io.TeeReader(tee, dst)
	}
	if deep {
		if err := checkStructure(tee, m, &res); err != nil {
			return res, err
		}
	}
	if _, err := io.Copy(io.Discard, tee); err != nil {
		if apperr.HasCode(err, "backup.wrongPassphrase") {
			return res, err
		}
		return res, apperr.Wrap(err, "backup.readFailed")
	}
	if m.ObjectSHA256 != "" && (objHC.Sum() != m.ObjectSHA256 || objHC.n != m.ObjectSize) {
		return res, checksumErr("object")
	}
	if payHC.Sum() != m.SHA256 || payHC.n != m.Size {
		return res, checksumErr("payload")
	}
	res.Size, res.SHA256, res.Format = payHC.n, payHC.Sum(), m.DumpFormat
	if j != nil {
		j.Logf("sha256 OK (%s, %s)", res.SHA256, humanBytes(res.Size))
	}
	return res, nil
}

// checkStructure decompresses the payload and checks the dump format.
func checkStructure(r io.Reader, m *Manifest, res *VerifyResult) error {
	var plain io.Reader = r
	switch m.Compression {
	case "gzip":
		zr, err := gzip.NewReader(r)
		if err != nil {
			return apperr.Wrap(err, "backup.corrupt")
		}
		defer zr.Close()
		plain = zr
	case "zstd":
		zr, err := zstd.NewReader(r)
		if err != nil {
			return apperr.Wrap(err, "backup.corrupt")
		}
		defer zr.Close()
		plain = zr
	}
	br := bufio.NewReaderSize(plain, 64<<10)
	switch m.DumpFormat {
	case "tar":
		tr := tar.NewReader(br)
		for {
			_, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return apperr.Wrap(err, "backup.corrupt")
			}
			res.Entries++
		}
	case "pg-custom":
		if head, _ := br.Peek(5); !bytes.Equal(head, []byte("PGDMP")) {
			return apperr.New("backup.corrupt").WithDetail("not a pg_dump custom-format archive")
		}
	case "rdb":
		if head, _ := br.Peek(5); !bytes.Equal(head, []byte("REDIS")) {
			return apperr.New("backup.corrupt").WithDetail("not a redis RDB file")
		}
	}
	// Drain the decompressor so its checksum is verified too.
	if _, err := io.Copy(io.Discard, br); err != nil {
		return apperr.Wrap(err, "backup.corrupt")
	}
	return nil
}

// download writes a verified copy of a backup to a local file.
func (s *BackupService) download(ctx context.Context, j *core.Job, sk sink, key string, m *Manifest, passphrase string, decrypt bool, local string) error {
	part := local + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return apperr.Wrap(err, "backup.writeFailed")
	}
	bw := bufio.NewWriterSize(f, 1<<20)
	fail := func(err error) error {
		f.Close()
		os.Remove(part)
		return err
	}
	pass := passphrase
	if !decrypt {
		pass = ""
	}
	mm := *m
	if !decrypt && m.Encrypted {
		// Raw copy: check the stored bytes only.
		if _, err := s.readRaw(ctx, sk, key, &mm, bw); err != nil {
			return fail(err)
		}
	} else if _, err := s.readVerified(ctx, j, sk, key, &mm, pass, false, bw, decrypt); err != nil {
		return fail(err)
	}
	if err := bw.Flush(); err != nil {
		return fail(apperr.Wrap(err, "backup.writeFailed"))
	}
	if err := f.Sync(); err != nil {
		return fail(apperr.Wrap(err, "backup.writeFailed"))
	}
	if err := f.Close(); err != nil {
		os.Remove(part)
		return apperr.Wrap(err, "backup.writeFailed")
	}
	if err := os.Rename(part, local); err != nil {
		os.Remove(part)
		return apperr.Wrap(err, "backup.writeFailed")
	}
	syncDir(filepath.Dir(local))
	return nil
}

// readRaw copies the stored bytes to dst and checks their hash.
func (s *BackupService) readRaw(ctx context.Context, sk sink, key string, m *Manifest, dst io.Writer) (int64, error) {
	r, err := sk.Open(ctx, key)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	hc := newHashCounter()
	n, err := io.Copy(io.MultiWriter(dst, hc), ctxReader{r: r, done: ctx.Err})
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return n, err
		}
		return n, apperr.Wrap(err, "backup.readFailed")
	}
	if m.ObjectSHA256 != "" && hc.Sum() != m.ObjectSHA256 {
		return n, checksumErr("object")
	}
	return n, nil
}
