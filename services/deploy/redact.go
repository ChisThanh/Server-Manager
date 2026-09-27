package deploy

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Mask replaces secret values in logs.
const Mask = "••••••"

// minSecretLen is the shortest value redacted; shorter values would wreck
// the log (secrets must be at least this long, see SaveApp).
const minSecretLen = 4

// Redactor replaces known secret values, and their common encodings, in
// text. It is safe for concurrent use.
type Redactor struct {
	mu       sync.RWMutex
	patterns []string // longest first
	first    [256][]string
	maxLen   int
}

// NewRedactor builds a redactor for the given secret values.
func NewRedactor(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add registers more secret values.
func (r *Redactor) Add(secrets ...string) {
	set := map[string]bool{}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.patterns {
		set[p] = true
	}
	for _, s := range secrets {
		for _, v := range variants(s) {
			set[v] = true
		}
	}
	r.patterns = r.patterns[:0]
	for p := range set {
		r.patterns = append(r.patterns, p)
	}
	sort.Slice(r.patterns, func(i, j int) bool {
		if len(r.patterns[i]) != len(r.patterns[j]) {
			return len(r.patterns[i]) > len(r.patterns[j])
		}
		return r.patterns[i] < r.patterns[j]
	})
	r.maxLen = 0
	r.first = [256][]string{}
	for _, p := range r.patterns {
		r.maxLen = max(r.maxLen, len(p))
		r.first[p[0]] = append(r.first[p[0]], p)
	}
}

// variants returns s and the encodings it may appear in: base64 (standard
// and URL alphabets, padded or not, at every byte alignment so a secret
// inside a larger encoded blob — e.g. "user:secret" in a Basic auth header —
// is caught), URL query/path escaping, JSON string escaping and hex.
func variants(s string) []string {
	if len(s) < minSecretLen {
		return nil
	}
	out := []string{s}
	add := func(v string) {
		if len(v) >= minSecretLen && v != s {
			out = append(out, v)
		}
	}
	// Each line of a multi-line secret separately (logs split lines).
	if strings.ContainsAny(s, "\r\n") {
		for _, l := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if len(strings.TrimSpace(l)) >= 8 {
				add(l)
			}
		}
	}
	b := []byte(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
		add(enc.EncodeToString(b))
		add(strings.TrimRight(enc.EncodeToString(b), "="))
		// Unaligned: s preceded by 1 or 2 bytes of something else. Keep only
		// the characters fully determined by s's bits.
		for off := 1; off <= 2; off++ {
			full := enc.WithPadding(base64.NoPadding).EncodeToString(append(make([]byte, off), b...))
			start := (8*off + 5) / 6
			end := 8 * (off + len(b)) / 6
			if end-start >= 6 && end <= len(full) {
				add(full[start:end])
			}
		}
		// Aligned but followed by more data: drop the trailing partial chars.
		full := enc.WithPadding(base64.NoPadding).EncodeToString(b)
		if end := 8 * len(b) / 6; end >= 6 {
			add(full[:end])
		}
	}
	add(url.QueryEscape(s))
	add(url.PathEscape(s))
	if j, err := json.Marshal(s); err == nil {
		add(string(j[1 : len(j)-1]))
	}
	add(hex.EncodeToString(b))
	add(strings.ToUpper(hex.EncodeToString(b)))
	return out
}

// Redact replaces every secret in s.
func (r *Redactor) Redact(s string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.patterns) == 0 || s == "" {
		return s
	}
	out, _ := r.scan([]byte(s), true)
	return string(out)
}

// scan redacts b. When final is false it stops before a suffix that could
// be the start of a secret, returning how many input bytes were consumed.
func (r *Redactor) scan(b []byte, final bool) ([]byte, int) {
	out := make([]byte, 0, len(b))
	i := 0
outer:
	for i < len(b) {
		cands := r.first[b[i]]
		if len(cands) > 0 {
			rest := b[i:]
			for _, p := range cands {
				if len(rest) >= len(p) && string(rest[:len(p)]) == p {
					out = append(out, Mask...)
					i += len(p)
					continue outer
				}
			}
			if !final && len(rest) < r.maxLen {
				for _, p := range cands {
					if len(p) > len(rest) && p[:len(rest)] == string(rest) {
						return out, i // might continue in the next chunk
					}
				}
			}
		}
		out = append(out, b[i])
		i++
	}
	return out, i
}

// Writer returns an io.Writer that redacts what is written before passing
// it to w. A possible secret split across writes is held back until it can
// be decided; call Flush when the stream ends. Use one Writer per stream
// (stdout and stderr separately) so chunks of different streams don't mix.
func (r *Redactor) Writer(w io.Writer) *RedactWriter {
	return &RedactWriter{r: r, w: w}
}

// RedactWriter is the streaming form of Redactor.
type RedactWriter struct {
	mu      sync.Mutex
	r       *Redactor
	w       io.Writer
	pending []byte
}

func (rw *RedactWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	buf := append(rw.pending, p...)
	rw.r.mu.RLock()
	out, n := rw.r.scan(buf, false)
	rw.r.mu.RUnlock()
	rw.pending = append([]byte(nil), buf[n:]...)
	if len(out) > 0 {
		if _, err := rw.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Flush writes what is held back (a partial secret prefix is masked).
func (rw *RedactWriter) Flush() {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if len(rw.pending) == 0 {
		return
	}
	rw.r.mu.RLock()
	out, _ := rw.r.scan(rw.pending, true)
	rw.r.mu.RUnlock()
	// A dangling prefix of a secret long enough to be meaningful is masked.
	if len(rw.pending) >= minSecretLen && string(out) == string(rw.pending) && rw.r.isPrefix(rw.pending) {
		out = []byte(Mask)
	}
	rw.pending = nil
	_, _ = rw.w.Write(out)
}

func (r *Redactor) isPrefix(b []byte) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.patterns {
		if len(p) > len(b) && p[:len(b)] == string(b) {
			return true
		}
	}
	return false
}
