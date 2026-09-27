package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"sync"

	"filippo.io/age"

	"server-manager/internal/apperr"
)

// scryptWorkFactor is age's default (≈1 s); tests lower it.
var scryptWorkFactor = 18

// encryptWriter returns a writer that encrypts into dst with a passphrase.
// Close finishes the age stream (it does not close dst).
func encryptWriter(dst io.Writer, passphrase string) (io.WriteCloser, error) {
	r, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return nil, apperr.Wrap(err, "backup.encryptFailed")
	}
	r.SetWorkFactor(scryptWorkFactor)
	w, err := age.Encrypt(dst, r)
	if err != nil {
		return nil, apperr.Wrap(err, "backup.encryptFailed")
	}
	return w, nil
}

// decryptReader returns the plaintext of an age stream.
func decryptReader(src io.Reader, passphrase string) (io.Reader, error) {
	if passphrase == "" {
		return nil, apperr.New("backup.noPassphrase")
	}
	id, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, apperr.Wrap(err, "backup.decryptFailed")
	}
	r, err := age.Decrypt(src, id)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) {
			return nil, apperr.New("backup.wrongPassphrase")
		}
		return nil, apperr.Wrap(err, "backup.decryptFailed")
	}
	return r, nil
}

// hashCounter hashes and counts everything written through it.
type hashCounter struct {
	h hash.Hash
	n int64
}

func newHashCounter() *hashCounter { return &hashCounter{h: sha256.New()} }

func (c *hashCounter) Write(p []byte) (int, error) {
	c.h.Write(p)
	c.n += int64(len(p))
	return len(p), nil
}

func (c *hashCounter) Sum() string { return hex.EncodeToString(c.h.Sum(nil)) }

// failWriter forwards writes to w; the first error is kept and onErr is
// called once (to stop the producer, e.g. cancel the SSH command).
type failWriter struct {
	w     io.Writer
	onErr func()
	mu    sync.Mutex
	err   error
}

func (f *failWriter) Write(p []byte) (int, error) {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return 0, err
	}
	f.mu.Unlock()
	n, err := f.w.Write(p)
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		f.mu.Lock()
		first := f.err == nil
		f.err = err
		f.mu.Unlock()
		if first && f.onErr != nil {
			f.onErr()
		}
	}
	return n, err
}

func (f *failWriter) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// tailBuffer keeps the last max bytes written.
type tailBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// ctxReader aborts reads once done reports an error.
type ctxReader struct {
	r    io.Reader
	done func() error
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.done(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
