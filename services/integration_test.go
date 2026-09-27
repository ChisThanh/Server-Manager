//go:build integration

// Integration tests against a real sshd. Start one with:
//
//	docker run -d --name sm-test-sshd -p 2222:2222 -e PASSWORD_ACCESS=true \
//	  -e USER_NAME=tester -e USER_PASSWORD=secret123 -e SUDO_ACCESS=true \
//	  lscr.io/linuxserver/openssh-server
//
// then run: go test -tags integration ./services/ -v
package services

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"server-manager/internal/apperr"
	icore "server-manager/internal/core"
	"server-manager/internal/sshx"
	"server-manager/internal/store"
)

const (
	testHost = "127.0.0.1"
	testUser = "tester"
	testPass = "secret123"
)

// testPort defaults to the Alpine container; SM_TEST_PORT points the suite
// at another sshd (e.g. a glibc distro) without code changes.
var testPort = func() int {
	if p, err := strconv.Atoi(os.Getenv("SM_TEST_PORT")); err == nil {
		return p
	}
	return 2222
}()

func setup(t *testing.T) (*Core, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // isolate config dir and known_hosts
	core, err := NewCore()
	if err != nil {
		t.Fatal(err)
	}
	ss := NewServerService(core)
	sv, err := ss.Save(store.Server{Host: testHost, Port: testPort, User: testUser, AuthType: store.AuthPassword}, "", false)
	if err != nil {
		t.Fatal(err)
	}

	res, err := ss.Connect(sv.ID, testPass, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "hostkey" || res.HostKey == nil || res.HostKey.Mismatch {
		t.Fatalf("want unknown hostkey prompt, got %+v", res)
	}
	if err := ss.TrustHostKey(res.HostKey.Host, res.HostKey.Port, res.HostKey.KeyBase64); err != nil {
		t.Fatal(err)
	}
	res, err = ss.Connect(sv.ID, testPass, false)
	if err != nil || res.Status != "ok" {
		t.Fatalf("connect after trust: %+v %v", res, err)
	}
	if res.Home == "" {
		t.Fatal("empty home")
	}
	t.Cleanup(func() { core.Manager.CloseAll() })
	return core, sv.ID
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestWrongPassword(t *testing.T) {
	core, _ := setup(t)
	ss := NewServerService(core)
	sv, _ := ss.Save(store.Server{Host: testHost, Port: testPort, User: testUser, AuthType: store.AuthPassword}, "", false)
	if _, err := ss.Connect(sv.ID, "wrong", false); !apperr.HasCode(err, "auth.failed") {
		t.Fatalf("want auth failure, got %v", err)
	}
	res, err := ss.Connect(sv.ID, "", false)
	if err != nil || res.Status != "need-secret" {
		t.Fatalf("want need-secret, got %+v %v", res, err)
	}
}

func TestFileOps(t *testing.T) {
	core, id := setup(t)
	fs := NewFileService(core)
	home, _ := fs.Home(id)
	base := home + "/sm-it-" + time.Now().Format("150405")
	t.Cleanup(func() { _ = fs.Delete(id, []string{base}) })

	must(t, fs.CreateDir(id, base+"/src/nested"))
	if err := fs.CreateDir(id, base+"/src"); err == nil {
		t.Fatal("CreateDir on existing dir should fail")
	}
	e, err := fs.CreateFile(id, base+"/src/main.go")
	must(t, err)
	if e.Size != 0 || e.IsDir {
		t.Fatalf("bad entry %+v", e)
	}
	if _, err := fs.CreateFile(id, base+"/src/main.go"); err == nil {
		t.Fatal("CreateFile on existing file should fail")
	}

	content := "package main\n\n// xin chào 👋\nfunc main() {}\n"
	r, err := fs.WriteFile(id, base+"/src/main.go", content, 0, false)
	must(t, err)
	if r.Conflict {
		t.Fatal("unexpected conflict")
	}
	fc, err := fs.ReadFile(id, base+"/src/main.go")
	must(t, err)
	if fc.Content != content || fc.Binary || fc.ReadOnly {
		t.Fatalf("read back mismatch: %+v", fc)
	}

	// Leftover temp files must not remain after a save.
	list, err := fs.ListDir(id, base+"/src")
	must(t, err)
	for _, x := range list {
		if strings.Contains(x.Name, ".sm-") {
			t.Fatalf("temp file left behind: %s", x.Name)
		}
	}
	if len(list) != 2 || !list[0].IsDir || list[0].Name != "nested" {
		t.Fatalf("unexpected listing (dirs first): %+v", list)
	}
	if list[1].Owner != testUser {
		t.Fatalf("owner name not resolved: %q", list[1].Owner)
	}

	// Conflict detection: file changed on the server after it was opened.
	time.Sleep(1100 * time.Millisecond)
	conn, _ := core.Conn(id)
	_, err = conn.Exec(t.Context(), "echo changed > "+base+"/src/main.go", nil)
	must(t, err)
	r, err = fs.WriteFile(id, base+"/src/main.go", "mine", fc.ModTime, false)
	must(t, err)
	if !r.Conflict {
		t.Fatal("expected conflict")
	}
	r, err = fs.WriteFile(id, base+"/src/main.go", "mine", fc.ModTime, true)
	must(t, err)
	if r.Conflict {
		t.Fatal("force should overwrite")
	}

	// In-place write keeps the inode (important for bind mounts) and mode.
	_, _ = conn.Exec(t.Context(), "chmod 750 "+base+"/src/main.go", nil)
	before, _ := conn.Exec(t.Context(), "stat -c %i "+base+"/src/main.go", nil)
	_, err = fs.WriteFile(id, base+"/src/main.go", "v2", 0, false)
	must(t, err)
	after, _ := conn.Exec(t.Context(), "stat -c '%i %a' "+base+"/src/main.go", nil)
	if strings.Fields(after.Stdout)[0] != strings.TrimSpace(before.Stdout) || strings.Fields(after.Stdout)[1] != "750" {
		t.Fatalf("inode/mode changed: before=%q after=%q", before.Stdout, after.Stdout)
	}

	// Writing through a symlink updates the target and keeps the link.
	_, _ = conn.Exec(t.Context(), "ln -s src/main.go "+base+"/link.go", nil)
	_, err = fs.WriteFile(id, base+"/link.go", "via link", 0, false)
	must(t, err)
	fc, _ = fs.ReadFile(id, base+"/src/main.go")
	if fc.Content != "via link" {
		t.Fatalf("symlink target not updated: %q", fc.Content)
	}
	st, _ := conn.Exec(t.Context(), "test -L "+base+"/link.go && echo link", nil)
	if strings.TrimSpace(st.Stdout) != "link" {
		t.Fatal("symlink was replaced by a regular file")
	}

	// Rename / copy / move.
	must(t, fs.Rename(id, base+"/src/main.go", base+"/src/app.go"))
	must(t, fs.Copy(id, []string{base + "/src/app.go"}, base+"/src"))
	if _, err := fs.Stat(id, base+"/src/app copy.go"); err != nil {
		t.Fatal("copy into same dir should create 'app copy.go'")
	}
	must(t, fs.Move(id, []string{base + "/src/app copy.go"}, base+"/src/nested"))
	if _, err := fs.Stat(id, base+"/src/nested/app copy.go"); err != nil {
		t.Fatal(err)
	}
	if err := fs.Move(id, []string{base + "/src"}, base+"/src/nested"); err == nil {
		t.Fatal("moving a dir into itself must fail")
	}

	// Search.
	hits, err := fs.Search(id, base, "via link", true)
	must(t, err)
	if len(hits) == 0 || hits[0].Line != 1 {
		t.Fatalf("content search: %+v", hits)
	}
	hits, err = fs.Search(id, base, "APP", false)
	must(t, err)
	if len(hits) != 2 {
		t.Fatalf("name search: %+v", hits)
	}

	// Archive round trip.
	must(t, fs.Compress(id, base, []string{"src"}, "src.tar.gz"))
	must(t, fs.CreateDir(id, base+"/x"))
	must(t, fs.Move(id, []string{base + "/src.tar.gz"}, base+"/x"))
	must(t, fs.Extract(id, base+"/x/src.tar.gz"))
	if _, err := fs.Stat(id, base+"/x/src/nested/app copy.go"); err != nil {
		t.Fatal("extract failed:", err)
	}

	// chmod.
	must(t, fs.Chmod(id, base+"/src/app.go", 0o600, false))
	e, _ = fs.Stat(id, base+"/src/app.go")
	if e.Perm != 0o600 {
		t.Fatalf("chmod: %o", e.Perm)
	}
	must(t, fs.Chmod(id, base+"/x", 0o700, true))

	// Binary detection.
	_, _ = conn.Exec(t.Context(), "head -c 2000 /dev/urandom > "+base+"/bin.dat", nil)
	fc, err = fs.ReadFile(id, base+"/bin.dat")
	must(t, err)
	if !fc.Binary {
		t.Fatal("binary not detected")
	}

	// Permission errors are readable.
	_, _ = conn.Exec(t.Context(), "echo secret > "+base+"/locked && chmod 000 "+base+"/locked", nil)
	if _, err := fs.ReadFile(id, base+"/locked"); !apperr.HasCode(err, "fs.permission") {
		t.Fatalf("want permission error, got %v", err)
	}

	must(t, fs.Delete(id, []string{base + "/x", base + "/bin.dat"}))
	if _, err := fs.Stat(id, base+"/x"); err == nil {
		t.Fatal("delete failed")
	}
	if err := fs.Delete(id, []string{"/"}); err == nil {
		t.Fatal("deleting / must be refused")
	}
}

func TestSudo(t *testing.T) {
	core, id := setup(t)
	fs := NewFileService(core)
	p := "/etc/sm-it-" + time.Now().Format("150405") + ".conf"
	conn, _ := core.Conn(id)
	t.Cleanup(func() { _, _ = sudoRun(context.Background(), conn, testPass, "rm -f "+p, "") })

	if _, err := fs.WriteFileSudo(id, p, "x", "", 0, false); err == nil {
		t.Fatal("sudo without password should fail")
	}
	if _, err := fs.WriteFileSudo(id, p, "x", "nope", 0, false); !apperr.HasCode(err, "sudo.wrongPassword") {
		t.Fatalf("want wrong password error, got %v", err)
	}
	content := "listen 80;\n# ghi bằng sudo\n"
	r, err := fs.WriteFileSudo(id, p, content, testPass, 0, false)
	must(t, err)
	fc, err := fs.ReadFileSudo(id, p, testPass)
	must(t, err)
	if fc.Content != content || !fc.Sudo || fc.ModTime != r.Entry.ModTime {
		t.Fatalf("sudo read back: %+v", fc)
	}
	owner, _ := conn.Exec(t.Context(), "stat -c %U "+p, nil)
	if strings.TrimSpace(owner.Stdout) != "root" {
		t.Fatalf("owner should stay root, got %q", owner.Stdout)
	}
	// Conflict detection through sudo.
	r2, err := fs.WriteFileSudo(id, p, "other", testPass, 12345, false)
	must(t, err)
	if !r2.Conflict {
		t.Fatal("expected sudo conflict")
	}
}

func TestTransfers(t *testing.T) {
	core, id := setup(t)
	fs := NewFileService(core)
	home, _ := fs.Home(id)
	remote := home + "/sm-tr-" + time.Now().Format("150405")
	must(t, fs.CreateDir(id, remote))
	t.Cleanup(func() { _ = fs.Delete(id, []string{remote}) })

	local := t.TempDir()
	src := filepath.Join(local, "proj")
	must(t, os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	big := make([]byte, 5<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	must(t, os.WriteFile(filepath.Join(src, "a", "b", "big.bin"), big, 0o644))
	must(t, os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755))

	waitDone := func(tid string) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			transfersMu.Lock()
			_, running := transfers[tid]
			transfersMu.Unlock()
			if !running {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("transfer timed out")
	}

	tid, err := fs.Upload(id, []string{src}, remote)
	must(t, err)
	waitDone(tid)
	e, err := fs.Stat(id, remote+"/proj/a/b/big.bin")
	must(t, err)
	if e.Size != int64(len(big)) {
		t.Fatalf("uploaded size %d", e.Size)
	}
	e, _ = fs.Stat(id, remote+"/proj/run.sh")
	if e.Perm&0o100 == 0 {
		t.Fatal("exec bit not preserved")
	}

	out := filepath.Join(t.TempDir(), "back")
	tid, err = fs.Download(id, remote+"/proj", out)
	must(t, err)
	waitDone(tid)
	got, err := os.ReadFile(filepath.Join(out, "a", "b", "big.bin"))
	must(t, err)
	if string(got) != string(big) {
		t.Fatal("downloaded content differs")
	}
}

func TestSystemAndTerminal(t *testing.T) {
	core, id := setup(t)
	sys := NewSystemService(core)
	info, err := sys.Info(id)
	must(t, err)
	if info.Hostname == "" || info.MemTotal == 0 || info.CPUs == 0 || len(info.Disks) == 0 {
		t.Fatalf("incomplete sys info: %+v", info)
	}
	procs, err := sys.Processes(id)
	must(t, err)
	if len(procs) == 0 {
		t.Fatal("no processes")
	}
	res, err := sys.Exec(id, "echo -n ok; echo err >&2; exit 3", false, "")
	must(t, err)
	if res.Stdout != "ok" || strings.TrimSpace(res.Stderr) != "err" || res.ExitCode != 3 {
		t.Fatalf("exec: %+v", res)
	}
	res, err = sys.Exec(id, "id -u", true, testPass)
	must(t, err)
	if strings.TrimSpace(res.Stdout) != "0" {
		t.Fatalf("sudo exec: %+v", res)
	}

	term := NewTerminalService(core)
	tid, err := term.Open(id, "/tmp", 100, 30)
	must(t, err)
	must(t, term.Resize(tid, 120, 40))
	must(t, term.Write(tid, "exit\n"))
	time.Sleep(500 * time.Millisecond)
	if err := term.Write(tid, "x"); err == nil {
		t.Fatal("terminal should be closed after exit")
	}
}

func TestAutoReconnect(t *testing.T) {
	core, id := setup(t)
	fs := NewFileService(core)
	home, _ := fs.Home(id)
	conn, _ := core.Conn(id)
	// Kill every sshd session of the test user, including ours.
	_, _ = conn.Exec(t.Context(), "pkill -u "+testUser+" -f 'sshd' || true", nil)
	time.Sleep(500 * time.Millisecond)
	if _, err := fs.ListDir(id, home); err != nil {
		t.Fatalf("operation after connection drop should reconnect: %v", err)
	}
	if !conn.Connected() {
		t.Fatal("should be connected again")
	}
}

func TestSudoFileOps(t *testing.T) {
	core, id := setup(t)
	fs := NewFileService(core)
	base := "/root-owned-" + time.Now().Format("150405")
	conn, _ := core.Conn(id)
	t.Cleanup(func() { _, _ = sudoRun(context.Background(), conn, testPass, "rm -rf "+base, "") })

	must(t, fs.SudoOp(id, "mkdir", []string{base + "/secret dir"}, "", 0, false, testPass))
	must(t, fs.SudoOp(id, "chmod", []string{base}, "", 0o700, true, testPass))
	must(t, fs.SudoOp(id, "touch", []string{base + "/a.txt"}, "", 0, false, testPass))
	if err := fs.SudoOp(id, "touch", []string{base + "/a.txt"}, "", 0, false, testPass); err == nil {
		t.Fatal("touch on existing file should fail")
	}
	if _, err := fs.ListDir(id, base); !apperr.HasCode(err, "fs.permission") {
		t.Fatalf("plain listing should be denied, got %v", err)
	}
	list, err := fs.ListDirSudo(id, base, testPass)
	must(t, err)
	if len(list) != 2 || !list[0].IsDir || list[0].Name != "secret dir" || list[1].Name != "a.txt" || list[1].Owner != "root" {
		t.Fatalf("sudo listing: %+v", list)
	}
	must(t, fs.SudoOp(id, "rename", []string{base + "/a.txt"}, base+"/b.txt", 0, false, testPass))
	must(t, fs.SudoOp(id, "copy", []string{base + "/b.txt"}, base+"/secret dir", 0, false, testPass))
	must(t, fs.SudoOp(id, "delete", []string{base + "/b.txt"}, "", 0, false, testPass))
	list, err = fs.ListDirSudo(id, base+"/secret dir", testPass)
	must(t, err)
	if len(list) != 1 || list[0].Name != "b.txt" {
		t.Fatalf("after ops: %+v", list)
	}
	if err := fs.SudoOp(id, "delete", []string{"/"}, "", 0, false, testPass); !apperr.HasCode(err, "fs.refuseRoot") {
		t.Fatal("must refuse deleting /")
	}
}

// TestTerminalUTF8 types Vietnamese into a real PTY the way an IME does
// (characters, then DEL to replace them) and checks the shell treats each
// DEL as one character, which only holds in a UTF-8 locale.
func TestTerminalUTF8(t *testing.T) {
	core, id := setup(t)
	var mu sync.Mutex
	outs := map[string]*strings.Builder{}
	hook := func(name string, data any) {
		if ev, ok := data.(TermDataEvent); ok && name == EventTermData {
			b, _ := base64.StdEncoding.DecodeString(ev.Data)
			mu.Lock()
			if outs[ev.ID] == nil {
				outs[ev.ID] = &strings.Builder{}
			}
			outs[ev.ID].Write(b)
			mu.Unlock()
		}
	}
	icore.SetEmitHook(hook)
	t.Cleanup(func() { icore.SetEmitHook(nil) })
	term := NewTerminalService(core)

	run := func(cwd, input string) string {
		t.Helper()
		tid, err := term.Open(id, cwd, 120, 40)
		must(t, err)
		defer term.Close(tid)
		time.Sleep(800 * time.Millisecond)
		must(t, term.Write(tid, input))
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			mu.Lock()
			got := ""
			if outs[tid] != nil {
				got = outs[tid].String()
			}
			mu.Unlock()
			// The echoed command line contains MARK too; the output is the last one.
			if i := strings.LastIndex(got, "MARK:"); i >= 0 && strings.Contains(got[i:], ":END") {
				return got[i : i+strings.Index(got[i:], ":END")+4]
			}
		}
		t.Fatal("no output from terminal")
		return ""
	}

	// "Việt" typed Telex-style: "Vie" then DEL + "ê", then "t", then DEL DEL + "ệt".
	line := run("", "echo MARK:$(locale charmap 2>/dev/null || echo ?):Vie\x7fê"+"t\x7f\x7fệt:END\n")
	t.Logf("login shell: %q", line)
	if !strings.HasSuffix(line, ":Việt:END") {
		t.Fatalf("shell mangled Vietnamese input: %q", line)
	}
	// "Open terminal here" goes through the /bin/sh wrapper.
	dir := "/tmp/sm dir 'q'"
	conn, _ := core.Conn(id)
	_, _ = conn.Exec(context.Background(), "mkdir -p "+sshx.ShellQuote(dir), nil)
	line = run(dir, "echo MARK:$(pwd):Vie\x7fê"+"t:END\n")
	t.Logf("cwd shell: %q", line)
	if line != "MARK:"+dir+":Viêt:END" {
		t.Fatalf("wrong cwd or input: %q", line)
	}
}

func TestTerminalRecording(t *testing.T) {
	core, id := setup(t)
	st := core.Settings()
	st.RecordTerminal = true
	if _, err := core.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var out strings.Builder
	exited := make(chan struct{}, 1)
	icore.SetEmitHook(func(name string, data any) {
		switch ev := data.(type) {
		case TermDataEvent:
			b, _ := base64.StdEncoding.DecodeString(ev.Data)
			mu.Lock()
			out.Write(b)
			mu.Unlock()
		case TermExitEvent:
			exited <- struct{}{}
		}
	})
	t.Cleanup(func() { icore.SetEmitHook(nil) })
	term := NewTerminalService(core)
	tid, err := term.Open(id, "", 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	must(t, term.Write(tid, "echo rec-$((6*7))\n"))
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		ok := strings.Contains(out.String(), "rec-42")
		mu.Unlock()
		if ok {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	must(t, term.Write(tid, "exit\n"))
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("terminal did not exit")
	}
	recs, err := term.Recordings(id)
	if err != nil || len(recs) != 1 {
		t.Fatalf("recordings: %+v %v", recs, err)
	}
	cast, err := term.ReadRecording(recs[0].File)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(cast), "\n")
	if !strings.HasPrefix(lines[0], `{"env"`) && !strings.Contains(lines[0], `"version":2`) {
		t.Fatalf("header: %s", lines[0])
	}
	if !strings.Contains(cast, "rec-42") || !strings.Contains(cast, `"o"`) {
		t.Fatalf("cast lacks output: %.400s", cast)
	}
	// Audit points at the recording; bad names are rejected.
	list, _ := core.AuditList(icore.AuditQuery{Server: id, Action: "terminal.open"})
	if len(list) == 0 || !strings.Contains(list[0].Detail, recs[0].File) {
		t.Fatalf("audit: %+v", list)
	}
	if _, err := term.ReadRecording("../../etc/passwd"); err == nil {
		t.Fatal("path traversal accepted")
	}
	must(t, term.DeleteRecording(recs[0].File))
	if recs, _ := term.Recordings(id); len(recs) != 0 {
		t.Fatal("not deleted")
	}
}
