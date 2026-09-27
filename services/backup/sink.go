package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

// objInfo is one object in a job folder.
type objInfo struct {
	Name     string
	Size     int64
	Modified time.Time
}

// objectWriter receives a new object. Nothing is visible under the final
// name until Commit succeeds; Abort discards the partial upload.
type objectWriter interface {
	io.Writer
	Commit() error
	Abort()
}

// sink is a backup destination. Keys are "<folder>/<name>" relative to the
// destination root.
type sink interface {
	Create(ctx context.Context, key string) (objectWriter, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// List returns the objects directly inside folder (missing = empty).
	List(ctx context.Context, folder string) ([]objInfo, error)
	Remove(ctx context.Context, key string) error
	PutSmall(ctx context.Context, key string, data []byte) error
	GetSmall(ctx context.Context, key string, max int64) ([]byte, error)
	// Describe renders a key for logs ("s3://bucket/prefix/key").
	Describe(key string) string
}

var errAborted = errors.New("aborted")

func isAbsLocal(p string) bool { return filepath.IsAbs(p) }

func destSecretName(id string) string { return "backup:dest:" + id + ":secret" }

// ---- local folder on this computer ----

type localSink struct{ base string }

func (l *localSink) path(key string) (string, error) {
	p := filepath.Join(l.base, filepath.FromSlash(key))
	rel, err := filepath.Rel(l.base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", apperr.New("backup.invalidKey")
	}
	return p, nil
}

func (l *localSink) Describe(key string) string {
	p, _ := l.path(key)
	return p
}

type localWriter struct {
	f         *os.File
	part, dst string
}

func (w *localWriter) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *localWriter) Commit() error {
	if err := w.f.Sync(); err != nil {
		w.Abort()
		return apperr.Wrap(err, "backup.writeFailed")
	}
	if err := w.f.Close(); err != nil {
		os.Remove(w.part)
		return apperr.Wrap(err, "backup.writeFailed")
	}
	if err := os.Rename(w.part, w.dst); err != nil {
		os.Remove(w.part)
		return apperr.Wrap(err, "backup.writeFailed")
	}
	syncDir(filepath.Dir(w.dst))
	return nil
}

func (w *localWriter) Abort() {
	w.f.Close()
	os.Remove(w.part)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func (l *localSink) Create(_ context.Context, key string) (objectWriter, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, apperr.Wrap(err, "backup.writeFailed")
	}
	part := p + ".part"
	_ = os.Remove(part)
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, apperr.Wrap(err, "backup.writeFailed")
	}
	return &localWriter{f: f, part: part, dst: p}, nil
}

func (l *localSink) Open(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, apperr.New("backup.objectNotFound", "key", key)
	}
	if err != nil {
		return nil, apperr.Wrap(err, "backup.readFailed")
	}
	return f, nil
}

func (l *localSink) List(_ context.Context, folder string) ([]objInfo, error) {
	p, err := l.path(folder)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(p)
	if errors.Is(err, fs.ErrNotExist) {
		return []objInfo{}, nil
	}
	if err != nil {
		return nil, apperr.Wrap(err, "backup.listFailed")
	}
	out := []objInfo{}
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, objInfo{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()})
	}
	return out, nil
}

func (l *localSink) Remove(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return apperr.Wrap(err, "backup.deleteFailed")
	}
	return nil
}

func (l *localSink) PutSmall(ctx context.Context, key string, data []byte) error {
	w, err := l.Create(ctx, key)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		w.Abort()
		return apperr.Wrap(err, "backup.writeFailed")
	}
	return w.Commit()
}

func (l *localSink) GetSmall(ctx context.Context, key string, max int64) ([]byte, error) {
	r, err := l.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, max))
}

// ---- directory on the server ----

type serverSink struct {
	s    *BackupService
	conn *sshx.Conn
	base string
	root bool
	pw   string
}

func (v *serverSink) abs(key string) string { return path.Join(v.base, key) }

func (v *serverSink) Describe(key string) string { return v.abs(key) }

type serverWriter struct {
	v    *serverSink
	ctx  context.Context
	pw   *io.PipeWriter
	done chan error
	part string
	dst  string
}

func (w *serverWriter) Write(p []byte) (int, error) { return w.pw.Write(p) }

func (w *serverWriter) Commit() error {
	w.pw.Close()
	if err := <-w.done; err != nil {
		w.cleanup()
		return err
	}
	ctx, cancel := context.WithTimeout(w.ctx, time.Minute)
	defer cancel()
	res, err := w.v.s.core.Run(ctx, w.v.conn, "mv -f -- "+core.Q(w.part)+" "+core.Q(w.dst), w.v.root, w.v.pw, "")
	if err == nil && res.ExitCode != 0 {
		err = core.CmdError(res)
	}
	if err != nil {
		w.cleanup()
		return apperr.Wrap(err, "backup.writeFailed")
	}
	return nil
}

func (w *serverWriter) Abort() {
	w.pw.CloseWithError(errAborted)
	<-w.done
	w.cleanup()
}

func (w *serverWriter) cleanup() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), 30*time.Second)
	defer cancel()
	_, _ = w.v.s.core.Run(ctx, w.v.conn, "rm -f -- "+core.Q(w.part), w.v.root, w.v.pw, "")
}

func (v *serverSink) Create(ctx context.Context, key string) (objectWriter, error) {
	dst := v.abs(key)
	part := dst + ".part"
	pr, pw := io.Pipe()
	w := &serverWriter{v: v, ctx: ctx, pw: pw, done: make(chan error, 1), part: part, dst: dst}
	script := "umask 077; mkdir -p -- " + core.Q(path.Dir(dst)) + " && cat > " + core.Q(part)
	go func() {
		var errBuf tailBuffer
		errBuf.max = 4096
		code, err := v.s.stream(ctx, v.conn, script, v.root, v.pw, pr, nil, &errBuf)
		if err == nil && code != 0 {
			err = apperr.New("backup.writeFailed").WithDetail(errBuf.String())
		}
		// Unblock the writer if the remote side stopped reading.
		if err != nil {
			pr.CloseWithError(err)
		} else {
			pr.CloseWithError(io.ErrClosedPipe)
		}
		w.done <- err
	}()
	return w, nil
}

type streamReader struct {
	*io.PipeReader
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *streamReader) Close() error {
	r.cancel()
	r.PipeReader.Close()
	<-r.done
	return nil
}

func (v *serverSink) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	p := v.abs(key)
	res, err := v.s.core.Run(ctx, v.conn, "test -f "+core.Q(p), v.root, v.pw, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, apperr.New("backup.objectNotFound", "key", key)
	}
	sctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	r := &streamReader{PipeReader: pr, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(r.done)
		var errBuf tailBuffer
		errBuf.max = 4096
		code, err := v.s.stream(sctx, v.conn, "cat -- "+core.Q(p), v.root, v.pw, nil, pw, &errBuf)
		if err == nil && code != 0 {
			err = apperr.New("backup.readFailed").WithDetail(errBuf.String())
		}
		pw.CloseWithError(err) // nil → EOF
	}()
	return r, nil
}

func (v *serverSink) List(ctx context.Context, folder string) ([]objInfo, error) {
	dir := v.abs(folder)
	script := "[ -d " + core.Q(dir) + " ] || exit 0; cd " + core.Q(dir) + " || exit 1; " +
		"for f in *; do [ -f \"$f\" ] && stat -c '%s/%Y/%n' -- \"$f\"; done; exit 0"
	res, err := v.s.core.Run(ctx, v.conn, script, v.root, v.pw, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, apperr.Wrap(core.CmdError(res), "backup.listFailed")
	}
	return parseStatList(res.Stdout), nil
}

// parseStatList parses "size/mtime/name" lines.
func parseStatList(out string) []objInfo {
	list := []objInfo{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "/", 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		size, err1 := strconv.ParseInt(parts[0], 10, 64)
		mt, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		list = append(list, objInfo{Name: parts[2], Size: size, Modified: time.Unix(mt, 0)})
	}
	return list
}

func (v *serverSink) Remove(ctx context.Context, key string) error {
	res, err := v.s.core.Run(ctx, v.conn, "rm -f -- "+core.Q(v.abs(key)), v.root, v.pw, "")
	if err == nil && res.ExitCode != 0 {
		err = core.CmdError(res)
	}
	if err != nil {
		return apperr.Wrap(err, "backup.deleteFailed")
	}
	return nil
}

func (v *serverSink) PutSmall(ctx context.Context, key string, data []byte) error {
	dst := v.abs(key)
	script := "umask 077; mkdir -p -- " + core.Q(path.Dir(dst)) + " && cat > " + core.Q(dst+".part") +
		" && mv -f -- " + core.Q(dst+".part") + " " + core.Q(dst)
	var errBuf tailBuffer
	errBuf.max = 4096
	code, err := v.s.stream(ctx, v.conn, script, v.root, v.pw, bytes.NewReader(data), nil, &errBuf)
	if err == nil && code != 0 {
		err = apperr.New("backup.writeFailed").WithDetail(errBuf.String())
	}
	return err
}

func (v *serverSink) GetSmall(ctx context.Context, key string, max int64) ([]byte, error) {
	p := v.abs(key)
	res, err := v.s.core.Run(ctx, v.conn, "[ -f "+core.Q(p)+" ] || exit 44; head -c "+strconv.FormatInt(max, 10)+" -- "+core.Q(p), v.root, v.pw, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 44 {
		return nil, apperr.New("backup.objectNotFound", "key", key)
	}
	if res.ExitCode != 0 {
		return nil, apperr.Wrap(core.CmdError(res), "backup.readFailed")
	}
	return []byte(res.Stdout), nil
}

// ---- S3-compatible object storage ----

// s3PartSize bounds memory use of streaming uploads (64 MiB × 10000 parts
// ≈ 640 GB maximum object size).
const s3PartSize = 64 << 20

type s3Sink struct {
	cl     *minio.Client
	bucket string
	prefix string
}

func newS3(d Destination, secret string) (*s3Sink, error) {
	if secret == "" {
		return nil, apperr.New("backup.s3.noSecret")
	}
	lookup := minio.BucketLookupAuto
	if d.PathStyle {
		lookup = minio.BucketLookupPath
	}
	cl, err := minio.New(d.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(d.AccessKey, secret, ""),
		Secure:       d.UseTLS,
		Region:       d.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, apperr.Wrap(err, "backup.s3.failed")
	}
	return &s3Sink{cl: cl, bucket: d.Bucket, prefix: d.Prefix}, nil
}

func (s *s3Sink) key(k string) string {
	if s.prefix == "" {
		return k
	}
	return s.prefix + "/" + k
}

func (s *s3Sink) Describe(key string) string { return "s3://" + s.bucket + "/" + s.key(key) }

// s3Err maps S3 errors to coded errors.
func s3Err(err error) error {
	if err == nil {
		return nil
	}
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	resp := minio.ToErrorResponse(err)
	switch resp.Code {
	case "NoSuchBucket":
		return apperr.New("backup.s3.noBucket").WithDetail(resp.Message)
	case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "InvalidToken", "ExpiredToken":
		return apperr.New("backup.s3.auth").WithDetail(fmt.Sprintf("%s: %s", resp.Code, resp.Message))
	case "NoSuchKey":
		return apperr.New("backup.objectNotFound", "key", resp.Key)
	}
	return apperr.Wrap(err, "backup.s3.failed")
}

type s3Writer struct {
	pw   *io.PipeWriter
	done chan error
}

func (w *s3Writer) Write(p []byte) (int, error) { return w.pw.Write(p) }

func (w *s3Writer) Commit() error {
	w.pw.Close()
	return <-w.done
}

func (w *s3Writer) Abort() {
	w.pw.CloseWithError(errAborted)
	<-w.done
}

func (s *s3Sink) Create(ctx context.Context, key string) (objectWriter, error) {
	pr, pw := io.Pipe()
	w := &s3Writer{pw: pw, done: make(chan error, 1)}
	// The upload keeps its own context so that an aborted stream (reader
	// error) can still abort the multipart upload on the server.
	uctx := context.WithoutCancel(ctx)
	go func() {
		_, err := s.cl.PutObject(uctx, s.bucket, s.key(key), pr, -1, minio.PutObjectOptions{
			PartSize:    s3PartSize,
			ContentType: "application/octet-stream",
		})
		if err != nil {
			pr.CloseWithError(err)
		} else {
			pr.CloseWithError(io.ErrClosedPipe)
		}
		w.done <- s3Err(err)
	}()
	return w, nil
}

func (s *s3Sink) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if _, err := s.cl.StatObject(ctx, s.bucket, s.key(key), minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, apperr.New("backup.objectNotFound", "key", key)
		}
		return nil, s3Err(err)
	}
	obj, err := s.cl.GetObject(ctx, s.bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, s3Err(err)
	}
	return obj, nil
}

func (s *s3Sink) List(ctx context.Context, folder string) ([]objInfo, error) {
	pfx := s.key(folder) + "/"
	out := []objInfo{}
	for o := range s.cl.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: pfx, Recursive: false}) {
		if o.Err != nil {
			return nil, s3Err(o.Err)
		}
		name := strings.TrimPrefix(o.Key, pfx)
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		out = append(out, objInfo{Name: name, Size: o.Size, Modified: o.LastModified})
	}
	return out, nil
}

func (s *s3Sink) Remove(ctx context.Context, key string) error {
	return s3Err(s.cl.RemoveObject(ctx, s.bucket, s.key(key), minio.RemoveObjectOptions{}))
}

func (s *s3Sink) PutSmall(ctx context.Context, key string, data []byte) error {
	_, err := s.cl.PutObject(ctx, s.bucket, s.key(key), bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/json"})
	return s3Err(err)
}

func (s *s3Sink) GetSmall(ctx context.Context, key string, max int64) ([]byte, error) {
	r, err := s.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, max))
	return b, s3Err(err)
}

// ---- construction ----

// sinkFor opens a destination. conn/root/pw are used by server destinations.
func (s *BackupService) sinkFor(d Destination, conn *sshx.Conn, root bool, pw string) (sink, error) {
	switch d.Type {
	case DestLocal:
		return &localSink{base: d.Path}, nil
	case DestServer:
		if conn == nil {
			return nil, apperr.New("conn.notConnected")
		}
		return &serverSink{s: s, conn: conn, base: d.Path, root: root, pw: pw}, nil
	case DestS3:
		return newS3(d, store.Keychain(destSecretName(d.ID)))
	}
	return nil, invalid("type")
}

// sortInfos sorts newest first by the time encoded in the name.
func sortInfos(list []objInfo) {
	sort.SliceStable(list, func(a, b int) bool { return list[a].Name > list[b].Name })
}
