//go:build integration

package core_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

func TestRunSudoAuditJobs(t *testing.T) {
	c, id := testutil.Connect(t, testutil.FullPort())
	ctx := context.Background()
	conn, err := c.Conn(id)
	if err != nil {
		t.Fatal(err)
	}
	// Plain run.
	res, err := c.Run(ctx, conn, "id -un", false, "", "")
	if err != nil || strings.TrimSpace(res.Stdout) != testutil.User {
		t.Fatalf("run: %+v %v", res, err)
	}
	// Sudo with the stored login password.
	res, err = c.Run(ctx, conn, "id -u", true, "", "")
	if err != nil || strings.TrimSpace(res.Stdout) != "0" {
		t.Fatalf("sudo: %+v %v", res, err)
	}
	// Auto: a root-only file.
	res, err = c.RunAuto(ctx, conn, "head -n1 /etc/shadow", "", "")
	if err != nil || !strings.HasPrefix(res.Stdout, "root:") {
		t.Fatalf("auto: %+v %v", res, err)
	}
	// Sudo stdin passthrough.
	res, err = c.Run(ctx, conn, "cat", true, "", "hello\n")
	if err != nil || res.Stdout != "hello\n" {
		t.Fatalf("sudo stdin: %q %v", res.Stdout, err)
	}
	// Wrong explicit password.
	c2, id2 := testutil.Connect(t, testutil.FullPort())
	conn2, _ := c2.Conn(id2)
	if _, err := c2.Run(ctx, conn2, "true", true, "nope", ""); err == nil || !strings.Contains(err.Error(), "wrong sudo") {
		t.Fatalf("want wrong password, got %v", err)
	}

	// Permission policy.
	if err := c.Require(id, core.PermSecurity); err != nil {
		t.Fatal(err)
	}
	testutil.SetRole(t, c, id, "viewer")
	if err := c.Require(id, core.PermFiles); err == nil {
		t.Fatal("viewer may not write files")
	}

	// Audit chain.
	c.Audit(id, "test.one", "a", "", nil)
	c.Audit(id, "test.two", "b", "detail", context.Canceled)
	list, err := c.AuditList(core.AuditQuery{Server: id})
	if err != nil || len(list) != 2 || list[0].Action != "test.two" || list[0].OK {
		t.Fatalf("audit list: %+v %v", list, err)
	}
	if v, _ := c.VerifyAudit(); !v.OK || v.Checked != 2 {
		t.Fatalf("verify: %+v", v)
	}
	_, _ = c.DB.Exec(`UPDATE audit SET target='x' WHERE action='test.one'`)
	if v, _ := c.VerifyAudit(); v.OK {
		t.Fatal("tampering not detected")
	}
	evs, _ := c.Events(core.EventQuery{Server: id})
	if len(evs) != 2 {
		t.Fatalf("events: %+v", evs)
	}

	// Jobs with streamed sudo output.
	j := c.Jobs.Start(id, "test", "job", func(ctx context.Context, j *core.Job) error {
		j.Step("step")
		return c.JobRun(ctx, j, conn, "echo out; echo err >&2; id -u", true, "")
	})
	info := j.Wait()
	if info.State != "done" || !strings.Contains(info.Log, "out") || !strings.Contains(info.Log, "err") || !strings.Contains(info.Log, "0") {
		t.Fatalf("job: %+v", info)
	}
	j = c.Jobs.Start(id, "test", "cancel", func(ctx context.Context, j *core.Job) error {
		return c.JobRun(ctx, j, conn, "sleep 30", false, "")
	})
	time.Sleep(300 * time.Millisecond)
	c.Jobs.Cancel(j.ID())
	if info := j.Wait(); info.State != "cancelled" {
		t.Fatalf("cancel: %+v", info)
	}

	// AnyConn opens a background connection with the stored secret.
	c.Manager.Disconnect(id)
	bg, err := c.AnyConn(ctx, id)
	if err != nil || !bg.Connected() {
		t.Fatalf("anyconn: %v", err)
	}
}
