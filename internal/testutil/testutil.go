// Package testutil connects integration tests to the Docker test servers
// (see testenv/README.md). Tests using it need the "integration" build tag.
package testutil

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

const (
	User     = "tester"
	Password = "secret123"
)

var mockOnce sync.Once

func port(env string, def int) int {
	if p, err := strconv.Atoi(os.Getenv(env)); err == nil {
		return p
	}
	return def
}

// FullPort is the Debian+systemd server (nginx, postgres, mariadb, redis,
// ufw, certbot…): SM_TEST_FULL_PORT, default 2225.
func FullPort() int { return port("SM_TEST_FULL_PORT", 2225) }

// DindPort is the Docker-in-Docker server: SM_TEST_DIND_PORT, default 2224.
func DindPort() int { return port("SM_TEST_DIND_PORT", 2224) }

// SecPort is the dedicated security test server (firewall/sshd tests):
// SM_TEST_SEC_PORT, default 2226.
func SecPort() int { return port("SM_TEST_SEC_PORT", 2226) }

// Connect builds an isolated Core (temp config dir, mock keychain, in-memory
// DB), saves a profile for tester@127.0.0.1:port with its password stored,
// trusts the host key and connects. Returns the core and the server id.
func Connect(t *testing.T, port int) (*core.Core, string) {
	t.Helper()
	mockOnce.Do(keyring.MockInit)
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := core.NewForTest(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	sv, err := st.Save(store.Server{Name: "test", Host: "127.0.0.1", Port: port, User: User, AuthType: store.AuthPassword}, Password, true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = c.Manager.Connect(ctx, sv, Password)
	var hk *sshx.HostKeyError
	if errors.As(err, &hk) {
		if err := c.Manager.HostKeys().Trust(hk.Host, hk.Port, hk.KeyBase64); err != nil {
			t.Fatal(err)
		}
		_, err = c.Manager.Connect(ctx, sv, Password)
	}
	if err != nil {
		t.Fatalf("connect 127.0.0.1:%d: %v (is the test container running? see testenv/README.md)", port, err)
	}
	t.Cleanup(c.Close)
	return c, sv.ID
}

// SetRole changes the server's role (to test permission checks).
func SetRole(t *testing.T, c *core.Core, id, role string) {
	t.Helper()
	sv, err := c.Store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	sv.Role = role
	if _, err := c.Store.Save(sv, "", false); err != nil {
		t.Fatal(err)
	}
}
