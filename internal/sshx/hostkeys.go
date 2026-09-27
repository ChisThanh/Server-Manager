package sshx

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyError is returned when the server's host key is unknown or differs
// from the one on record. The UI asks the user to confirm before trusting it.
type HostKeyError struct {
	Host        string
	Port        int
	KeyType     string
	Fingerprint string
	KeyBase64   string
	Mismatch    bool
}

func (e *HostKeyError) Error() string {
	if e.Mismatch {
		return fmt.Sprintf("host key for %s has CHANGED (%s %s)", e.Host, e.KeyType, e.Fingerprint)
	}
	return fmt.Sprintf("host %s is not trusted yet (%s %s)", e.Host, e.KeyType, e.Fingerprint)
}

// HostKeys verifies server keys against the app's own known_hosts file plus
// the user's ~/.ssh/known_hosts (read-only).
type HostKeys struct {
	mu      sync.Mutex
	appFile string
}

func NewHostKeys(configDir string) *HostKeys {
	return &HostKeys{appFile: filepath.Join(configDir, "known_hosts")}
}

func (h *HostKeys) files() []string {
	files := []string{}
	if _, err := os.Stat(h.appFile); err == nil {
		files = append(files, h.appFile)
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".ssh", "known_hosts")
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	return files
}

func (h *HostKeys) Callback(host string, port int) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		h.mu.Lock()
		files := h.files()
		h.mu.Unlock()

		hkErr := &HostKeyError{
			Host:        host,
			Port:        port,
			KeyType:     key.Type(),
			Fingerprint: ssh.FingerprintSHA256(key),
			KeyBase64:   base64.StdEncoding.EncodeToString(key.Marshal()),
		}
		if len(files) == 0 {
			return hkErr
		}
		cb, err := knownhosts.New(files...)
		if err != nil {
			// A malformed user known_hosts shouldn't block us; retry with ours only.
			if _, statErr := os.Stat(h.appFile); statErr != nil {
				return hkErr
			}
			if cb, err = knownhosts.New(h.appFile); err != nil {
				return hkErr
			}
		}
		err = cb(hostname, remote, key)
		if err == nil {
			return nil
		}
		var keyErr *knownhosts.KeyError
		if errors.As(err, &keyErr) {
			hkErr.Mismatch = len(keyErr.Want) > 0
			return hkErr
		}
		return err
	}
}

// KnownAlgorithms returns the host key algorithms already on record for the
// address, so the handshake negotiates a key type we can actually verify
// instead of reporting a false "key changed" for a different key type.
func (h *HostKeys) KnownAlgorithms(hostname string, remote net.Addr) []string {
	h.mu.Lock()
	files := h.files()
	h.mu.Unlock()
	if len(files) == 0 {
		return nil
	}
	cb, err := knownhosts.New(files...)
	if err != nil {
		return nil
	}
	var keyErr *knownhosts.KeyError
	if err := cb(hostname, remote, probeKey{}); !errors.As(err, &keyErr) {
		return nil
	}
	seen := map[string]bool{}
	var algos []string
	add := func(a ...string) {
		for _, x := range a {
			if !seen[x] {
				seen[x] = true
				algos = append(algos, x)
			}
		}
	}
	for _, k := range keyErr.Want {
		switch t := k.Key.Type(); t {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
		default:
			add(t)
		}
	}
	return algos
}

// probeKey never matches a real key; it is used to list known keys for a host.
type probeKey struct{}

func (probeKey) Type() string                        { return "probe" }
func (probeKey) Marshal() []byte                     { return []byte("server-manager-probe") }
func (probeKey) Verify([]byte, *ssh.Signature) error { return errors.New("probe") }

// Trust records the key for host:port in the app's known_hosts file,
// replacing any previous entry for that address.
func (h *HostKeys) Trust(host string, port int, keyBase64 string) error {
	raw, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return err
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	addr := knownhosts.Normalize(net.JoinHostPort(host, fmt.Sprint(port)))
	var kept []string
	if data, err := os.ReadFile(h.appFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) > 0 && hostListContains(fields[0], addr) {
				continue
			}
			kept = append(kept, line)
		}
	}
	kept = append(kept, knownhosts.Line([]string{addr}, key))
	return os.WriteFile(h.appFile, []byte(strings.Join(kept, "\n")+"\n"), 0o600)
}

func hostListContains(list, addr string) bool {
	for _, h := range strings.Split(list, ",") {
		if h == addr {
			return true
		}
	}
	return false
}

// Fingerprint returns the SHA256 fingerprint of a base64 public key, or "".
func Fingerprint(keyBase64 string) string {
	raw, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return ""
	}
	key, err := ssh.ParsePublicKey(raw)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}
