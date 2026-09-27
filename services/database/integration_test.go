//go:build integration

// Integration tests against the Debian test server (testutil.FullPort):
// PostgreSQL 15 (peer auth), MariaDB (root via unix_socket) and Redis run
// locally. Run: go test -tags integration -race -count=1 ./services/database/ -v
package database

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/testutil"
)

func setup(t *testing.T) (*DatabaseService, *core.Core, string) {
	t.Helper()
	c, id := testutil.Connect(t, testutil.FullPort())
	return New(c), c, id
}

// must(f())(t) fails the test on error.
func must[T any](v T, err error) func(t *testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatalf("%v", err)
		}
		return v
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if !apperr.HasCode(err, code) {
		t.Fatalf("want %s, got %v", code, err)
	}
}

// admin runs trusted SQL as the local admin (cleanup helper).
func admin(t *testing.T, s *DatabaseService, id, target, sql string) {
	t.Helper()
	_, err := s.RunQuery(id, QueryRequest{TargetID: target, SQL: sql, AllowWrite: true}, "")
	if err != nil {
		t.Logf("cleanup %q: %v", sql, err)
	}
}

func TestDetect(t *testing.T) {
	s, _, id := setup(t)
	d := must(s.Detect(id, ""))(t)
	byID := map[string]Target{}
	for _, tg := range d.Targets {
		byID[tg.ID] = tg
	}
	for _, want := range []string{"local-postgres", "local-mysql", "local-redis"} {
		tg, ok := byID[want]
		if !ok {
			t.Fatalf("%s not detected: %+v", want, d.Targets)
		}
		if !tg.Running || tg.Version == "" || tg.Auth != AuthPeer {
			t.Errorf("%s: %+v", want, tg)
		}
	}
	if byID["local-mysql"].Name != "MariaDB" {
		t.Errorf("mysql flavor: %+v", byID["local-mysql"])
	}
	for _, e := range d.Engines {
		if e.Client == "" || !e.Installed {
			t.Errorf("engine %+v", e)
		}
	}
	t.Logf("docker=%s engines=%+v", d.Docker, d.Engines)
	list := must(s.Targets(id))(t)
	if len(list) < 3 {
		t.Fatalf("targets: %+v", list)
	}
}

func TestOverviews(t *testing.T) {
	s, _, id := setup(t)
	pg := must(s.Overview(id, "local-postgres", ""))(t)
	p := pg.Postgres
	if !strings.HasPrefix(pg.Version, "15") || p.VersionNum < 150000 || pg.Uptime <= 0 || p.MaxConnections <= 0 || p.Connections < 1 {
		t.Fatalf("pg overview: %+v %+v", pg, p)
	}
	found := false
	for _, d := range p.Databases {
		if d.Name == "postgres" && d.Size > 0 && d.Owner == "postgres" && d.Encoding != "" {
			found = true
		}
	}
	if !found || p.CacheHitRatio < 0 || p.CacheHitRatio > 1 || len(p.Settings) == 0 || len(p.ByState) == 0 {
		t.Fatalf("pg databases/ratio: %+v", p)
	}

	my := must(s.Overview(id, "local-mysql", ""))(t)
	m := my.MySQL
	if m.Flavor != "mariadb" || !strings.Contains(my.Version, "MariaDB") || my.Uptime <= 0 || m.MaxConnections <= 0 || m.ThreadsConnected < 1 || m.Databases < 3 {
		t.Fatalf("mysql overview: %+v %+v", my, m)
	}
	if m.BufferPoolSize <= 0 || m.BufferPoolHitRatio < 0 || m.SlowLog.LongQueryTime <= 0 {
		t.Fatalf("mysql innodb/slowlog: %+v", m)
	}

	rd := must(s.Overview(id, "local-redis", ""))(t)
	r := rd.Redis
	if !strings.HasPrefix(rd.Version, "7") || rd.Uptime <= 0 || r.ConnectedClients < 1 || r.UsedMemory <= 0 || r.MaxMemoryPolicy == "" || r.Role != "master" || len(r.Sections) < 3 {
		t.Fatalf("redis overview: %+v %+v", rd, r)
	}
	if r.SlowlogError != "" || r.ConfigError != "" {
		t.Fatalf("redis slowlog/config: %+v", r)
	}

	for _, tg := range []string{"local-postgres", "local-mysql"} {
		dbs := must(s.Databases(id, tg, ""))(t)
		if len(dbs) == 0 {
			t.Fatalf("%s: no databases", tg)
		}
		acts := must(s.Activity(id, tg, ""))(t)
		if acts.MaxConnections <= 0 {
			t.Fatalf("%s activity: %+v", tg, acts)
		}
		slow := must(s.SlowQueries(id, tg, "", "total", ""))(t)
		t.Logf("%s slow: source=%s reason=%s slowlog=%+v", tg, slow.Source, slow.Reason, slow.SlowLog)
		if slow.Source == "none" && slow.Reason == "" {
			t.Fatalf("%s: slow report without reason", tg)
		}
	}
	tables := must(s.Tables(id, "local-mysql", "mysql", ""))(t)
	if len(tables) == 0 || tables[0].Schema != "mysql" {
		t.Fatalf("mysql tables: %+v", tables)
	}
	must(s.Tables(id, "local-postgres", "postgres", ""))(t)
	v := must(s.Ping(id, "local-redis", ""))(t)
	if !strings.HasPrefix(v, "7") {
		t.Fatalf("ping: %q", v)
	}
}

// tricky values that must round-trip exactly.
var tricky = []string{"", "a,b", "tab\there", "line1\nline2", "cr\r\nlf", `quote"double`, "quote'single", `back\slash`,
	"unicode ệ 漢字 🎉", "NULL", `\N`, "  spaces  ", "<xml>&amp;</xml>", "@@SM", "a\x01b"}

func trickySQL(pg bool) string {
	var cols []string
	for i, v := range tricky {
		lit := "'" + strings.ReplaceAll(v, "'", "''") + "'"
		if pg {
			if strings.ContainsAny(v, "\\\n\r\t\x01") {
				lit = "E'" + strings.NewReplacer(`\`, `\\`, "'", `\'`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\x01", `\x01`).Replace(v) + "'"
			}
		} else {
			lit = "'" + strings.NewReplacer(`\`, `\\`, "'", `\'`, "\n", `\n`, "\r", `\r`, "\t", `\t`, "\x01", `\Z`).Replace(v) + "'"
			if strings.Contains(v, "\x01") {
				lit = "CONCAT('a', CHAR(1), 'b')"
			}
		}
		cols = append(cols, fmt.Sprintf("%s AS c%d", lit, i))
	}
	cols = append(cols, "NULL AS cnull")
	return "SELECT " + strings.Join(cols, ", ")
}

func checkTricky(t *testing.T, st StatementResult) {
	t.Helper()
	if !st.HasRows || len(st.Rows) != 1 || len(st.Columns) != len(tricky)+1 {
		t.Fatalf("shape: %+v", st)
	}
	row := st.Rows[0]
	for i, v := range tricky {
		if row[i] == nil || *row[i] != v {
			got := "<nil>"
			if row[i] != nil {
				got = *row[i]
			}
			t.Errorf("col %d: got %q want %q", i, got, v)
		}
		if st.Columns[i] != fmt.Sprintf("c%d", i) {
			t.Errorf("column name %q", st.Columns[i])
		}
	}
	if row[len(tricky)] != nil {
		t.Errorf("NULL came back as %q", *row[len(tricky)])
	}
}

func TestQueryRunnerTrickyValues(t *testing.T) {
	s, _, id := setup(t)
	for _, tg := range []string{"local-postgres", "local-mysql"} {
		t.Run(tg, func(t *testing.T) {
			pg := tg == "local-postgres"
			sql := trickySQL(pg) + ";\n-- comment ; here\nSELECT 1 AS one WHERE 1 = 0;\nSELECT 'x;y' AS \"semi\""
			if !pg {
				sql = strings.ReplaceAll(sql, `"semi"`, "`semi`")
			}
			res := must(s.RunQuery(id, QueryRequest{TargetID: tg, SQL: sql}, ""))(t)
			if res.ErrorIndex != 0 || len(res.Statements) != 3 || !res.ReadOnly {
				t.Fatalf("result: %+v", res)
			}
			checkTricky(t, res.Statements[0])
			if st := res.Statements[1]; !st.HasRows || len(st.Rows) != 0 || st.RowCount != 0 {
				t.Fatalf("empty result: %+v", st)
			}
			if st := res.Statements[2]; *st.Rows[0][0] != "x;y" || st.Columns[0] != "semi" {
				t.Fatalf("third: %+v", st)
			}
			// row cap
			q := "SELECT generate_series(1, 1500) AS n"
			if !pg {
				q = "WITH d AS (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 " +
					"UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) " +
					"SELECT a.n * 1000 + b.n * 100 + c.n * 10 + e.n + 1 AS n FROM d a, d b, d c, d e HAVING n <= 1500 ORDER BY n"
			}
			res = must(s.RunQuery(id, QueryRequest{TargetID: tg, SQL: q}, ""))(t)
			st := res.Statements[0]
			if len(st.Rows) != 1000 || !st.Truncated || st.RowCount != 1500 || *st.Rows[999][0] != "1000" {
				t.Fatalf("cap: rows=%d truncated=%v count=%d", len(st.Rows), st.Truncated, st.RowCount)
			}
			// statement error keeps earlier results
			res = must(s.RunQuery(id, QueryRequest{TargetID: tg, SQL: "SELECT 1 AS a; SELECT * FROM smtest_db_missing_table"}, ""))(t)
			if res.ErrorIndex != 2 || len(res.Statements) != 1 || res.Error == "" {
				t.Fatalf("error: %+v", res)
			}
			t.Logf("error text: %s", res.Error)
			// meta-commands never reach the client
			_, err := s.RunQuery(id, QueryRequest{TargetID: tg, SQL: "SELECT 1; \\! id"}, "")
			wantCode(t, err, "db.metaCommand")
			hist := must(s.History(id, tg))(t)
			if len(hist) < 3 {
				t.Fatalf("history: %+v", hist)
			}
		})
	}
}

func TestReadOnlyAndWrite(t *testing.T) {
	s, _, id := setup(t)
	name := fmt.Sprintf("smtest_db_%d", time.Now().UnixNano()%1_000_000)
	for _, tg := range []string{"local-postgres", "local-mysql"} {
		t.Run(tg, func(t *testing.T) {
			pg := tg == "local-postgres"
			_, err := s.RunQuery(id, QueryRequest{TargetID: tg, SQL: "CREATE DATABASE " + name}, "")
			wantCode(t, err, "db.readOnly")
			_, err = s.RunQuery(id, QueryRequest{TargetID: tg, SQL: "SELECT 1; COMMIT; DROP DATABASE postgres"}, "")
			wantCode(t, err, "db.readOnly")

			if err := s.CreateDatabase(id, tg, name, "", ""); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { admin(t, s, id, tg, "DROP DATABASE IF EXISTS "+name) })
			dbs := must(s.Databases(id, tg, ""))(t)
			found := false
			for _, d := range dbs {
				found = found || d.Name == name
			}
			if !found {
				t.Fatalf("%s not listed", name)
			}
			create := "CREATE TABLE items (id int PRIMARY KEY, label text)"
			if !pg {
				create = "CREATE TABLE items (id int PRIMARY KEY, label text) ENGINE=InnoDB"
			}
			res := must(s.RunQuery(id, QueryRequest{TargetID: tg, Database: name, AllowWrite: true,
				SQL: create + "; INSERT INTO items VALUES (1, 'a'), (2, NULL), (3, 'c'); UPDATE items SET label = 'z' WHERE id > 1"}, ""))(t)
			if res.ErrorIndex != 0 || len(res.Statements) != 3 || res.Statements[1].RowCount != 3 || res.Statements[2].RowCount != 2 {
				t.Fatalf("write: %+v", res)
			}
			// read-only mode: server-side guard too
			if pg {
				res = must(s.RunQuery(id, QueryRequest{TargetID: tg, Database: name,
					SQL: "WITH d AS (DELETE FROM items RETURNING 1) SELECT count(*) FROM d"}, ""))(t)
				if res.ErrorIndex != 1 || !strings.Contains(res.Error, "read-only") {
					t.Fatalf("read-only guard: %+v", res)
				}
			}
			res = must(s.RunQuery(id, QueryRequest{TargetID: tg, Database: name, SQL: "SELECT id, label FROM items ORDER BY id"}, ""))(t)
			if st := res.Statements[0]; len(st.Rows) != 3 || *st.Rows[1][1] != "z" {
				t.Fatalf("select: %+v", res)
			}
			tables := must(s.Tables(id, tg, name, ""))(t)
			if len(tables) != 1 || tables[0].Name != "items" {
				t.Fatalf("tables: %+v", tables)
			}
		})
	}
}

func TestActivityCancel(t *testing.T) {
	s, _, id := setup(t)
	cases := map[string]string{"local-postgres": "SELECT pg_sleep(60) AS smtest_db_sleep", "local-mysql": "SELECT SLEEP(60) AS smtest_db_sleep"}
	for tg, q := range cases {
		t.Run(tg, func(t *testing.T) {
			var wg sync.WaitGroup
			var res QueryResult
			var qerr error
			start := time.Now()
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, qerr = s.RunQuery(id, QueryRequest{TargetID: tg, SQL: q, TimeoutSec: 90}, "")
			}()
			var sid int64
			for i := 0; i < 40 && sid == 0; i++ {
				time.Sleep(500 * time.Millisecond)
				a := must(s.Activity(id, tg, ""))(t)
				for _, se := range a.Sessions {
					// psql fetches through a cursor: the running statement is its FETCH
					if strings.Contains(se.Query, "smtest_db_sleep") || (se.App == "server-manager-query" && se.State == "active") {
						sid = se.ID
					}
				}
			}
			if sid == 0 {
				t.Fatal("sleeping session not found")
			}
			if err := s.CancelSession(id, tg, sid, false, ""); err != nil {
				t.Fatal(err)
			}
			wg.Wait()
			if qerr != nil {
				t.Fatal(qerr)
			}
			if time.Since(start) > 30*time.Second {
				t.Fatalf("query not cancelled (%v)", time.Since(start))
			}
			t.Logf("cancelled result: errIndex=%d error=%q", res.ErrorIndex, res.Error)
			if tg == "local-postgres" && !strings.Contains(res.Error, "canceling statement") {
				t.Fatalf("pg cancel: %+v", res)
			}
			wantCode(t, s.CancelSession(id, tg, 0, false, ""), "db.invalidPid")
		})
	}
}

func TestRedis(t *testing.T) {
	s, c, id := setup(t)
	key := fmt.Sprintf("smtest-db-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = s.RedisCommand(id, "local-redis", 0, "DEL "+key, true, "") })
	_, err := s.RedisCommand(id, "local-redis", 0, `SET `+key+` "v"`, false, "")
	wantCode(t, err, "db.redis.needConfirm")
	r := must(s.RedisCommand(id, "local-redis", 0, `SET `+key+` "tab\there \"q\" ệ"`, true, ""))(t)
	if r.Output != "OK" || !r.Write {
		t.Fatalf("set: %+v", r)
	}
	r = must(s.RedisCommand(id, "local-redis", 0, "GET "+key, false, ""))(t)
	if unrepr(r.Output) != "tab\there \"q\" ệ" {
		t.Fatalf("get: %q", r.Output)
	}
	r = must(s.RedisCommand(id, "local-redis", 0, "HGETALL smtest-db-none", false, ""))(t)
	if r.Output != "(empty array)" && r.Output != "(empty list or set)" {
		t.Fatalf("hgetall: %q", r.Output)
	}
	r = must(s.RedisCommand(id, "local-redis", 0, "INCR "+key, true, ""))(t)
	if !r.IsError {
		t.Fatalf("incr on string should fail: %+v", r)
	}
	_, err = s.RedisCommand(id, "local-redis", 0, "FLUSHALL", false, "")
	wantCode(t, err, "db.redis.needConfirm")
	_, err = s.RedisCommand(id, "local-redis", 0, "MONITOR", true, "")
	wantCode(t, err, "db.redis.blocked")
	_, err = s.RedisCommand(id, "local-redis", 0, `GET "unterminated`, false, "")
	wantCode(t, err, "db.redis.badQuote")
	if n := must(s.RedisCommand(id, "local-redis", 0, "EXISTS "+key, false, ""))(t); n.Output != "(integer) 1" {
		t.Fatalf("key vanished (FLUSHALL ran?): %+v", n)
	}
	sl := must(s.RedisCommand(id, "local-redis", 0, "SLOWLOG GET 5", false, ""))(t)
	if sl.IsError {
		t.Fatalf("slowlog: %+v", sl)
	}
	// audit
	entries, _ := c.AuditList(core.AuditQuery{Server: id, Action: "db.redis."})
	if len(entries) < 3 {
		t.Fatalf("audit: %+v", entries)
	}
	for _, e := range entries {
		if strings.Contains(e.Action, "FLUSHALL") || strings.Contains(e.Detail, "FLUSHALL") {
			t.Fatalf("refused command audited as run: %+v", e)
		}
	}
}

func TestPasswordTargets(t *testing.T) {
	s, c, id := setup(t)
	const user = "smtest_db_user"
	pw := fmt.Sprintf("Pw'x\"$(id)`;%d", time.Now().UnixNano()%100000)
	var mu sync.Mutex
	var cmds []string
	traceHook = func(cmd string) { mu.Lock(); cmds = append(cmds, cmd); mu.Unlock() }
	t.Cleanup(func() { traceHook = nil })

	cases := []struct{ admin, engine, host, drop string }{
		{"local-postgres", EnginePostgres, "127.0.0.1", "DROP ROLE IF EXISTS " + user},
		{"local-mysql", EngineMySQL, "localhost", "DROP USER IF EXISTS '" + user + "'@'localhost'"},
	}
	for _, cs := range cases {
		t.Run(cs.engine, func(t *testing.T) {
			admin(t, s, id, cs.admin, cs.drop)
			t.Cleanup(func() { admin(t, s, id, cs.admin, cs.drop) })
			if err := s.CreateUser(id, cs.admin, user, pw, "", "", ""); err != nil {
				t.Fatal(err)
			}
			tg := must(s.SaveTarget(id, TargetInput{Engine: cs.engine, Name: "smtest pw", Mode: ModeLocal, Auth: AuthPassword,
				Host: cs.host, User: user}, pw, true))(t)
			t.Cleanup(func() { _ = s.DeleteTarget(id, tg.ID) })
			if !tg.HasPassword || !tg.Saved {
				t.Fatalf("target: %+v", tg)
			}
			// the query runs long enough to look at the process list meanwhile
			sleep := "SELECT current_user AS u, pg_sleep(2) AS s"
			if cs.engine == EngineMySQL {
				sleep = "SELECT current_user() AS u, SLEEP(2) AS s"
			}
			done := make(chan QueryResult, 1)
			go func() {
				r, err := s.RunQuery(id, QueryRequest{TargetID: tg.ID, SQL: sleep}, "")
				if err != nil {
					t.Error(err)
				}
				done <- r
			}()
			time.Sleep(1200 * time.Millisecond)
			conn := must(c.Conn(id))(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			ps, err := c.Run(ctx, conn, "ps -eo args", true, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(ps.Stdout, pw) || strings.Contains(ps.Stdout, "Pw'x") {
				t.Fatal("password visible in the process list")
			}
			res := <-done
			if res.ErrorIndex != 0 || len(res.Statements) != 1 || !strings.HasPrefix(*res.Statements[0].Rows[0][0], user) {
				t.Fatalf("password login: %+v", res)
			}
			// wrong password → coded auth error
			must(s.SaveTarget(id, TargetInput{ID: tg.ID, Engine: cs.engine, Name: "smtest pw", Mode: ModeLocal, Auth: AuthPassword,
				Host: cs.host, User: user}, "wrong-password", true))(t)
			_, err = s.RunQuery(id, QueryRequest{TargetID: tg.ID, SQL: "SELECT 1"}, "")
			wantCode(t, err, "db.authFailed")
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cmds) == 0 {
		t.Fatal("no commands traced")
	}
	for _, cmd := range cmds {
		if strings.Contains(cmd, pw) || strings.Contains(cmd, "wrong-password") {
			t.Fatalf("password on a command line: %s", cmd)
		}
	}
	// audit never contains the password
	entries, _ := c.AuditList(core.AuditQuery{Server: id})
	for _, e := range entries {
		if strings.Contains(e.Detail+e.Target+e.Error, pw) {
			t.Fatalf("password in audit: %+v", e)
		}
	}
}

func TestViewerAndInvalidInput(t *testing.T) {
	s, _, id := setup(t)
	wantCode(t, s.CreateDatabase(id, "local-postgres", "bad;name", "", ""), "db.invalidName")
	wantCode(t, s.CreateDatabase(id, "local-mysql", "smtest_db_ok", "x\"y", ""), "db.invalidName")
	_, err := s.Tables(id, "local-postgres", "x' OR 1=1", "")
	wantCode(t, err, "db.invalidName")
	_, err = s.Overview(id, "nope/../x", "")
	wantCode(t, err, "db.targetNotFound")
	_, err = s.SaveTarget(id, TargetInput{Engine: EngineMySQL, Mode: ModeDocker, Container: "a b", Auth: AuthPeer}, "", false)
	wantCode(t, err, "db.invalidContainer")
	_, err = s.SaveTarget(id, TargetInput{Engine: EnginePostgres, Mode: ModeLocal, Auth: AuthPassword, User: "u", Host: "h;rm"}, "", false)
	wantCode(t, err, "db.invalidHost")
	_, err = s.RunQuery(id, QueryRequest{TargetID: "local-postgres", SQL: "SELECT 1", Database: "-h evil"}, "")
	wantCode(t, err, "db.invalidName")

	testutil.SetRole(t, s.core, id, "viewer")
	_, err = s.RunQuery(id, QueryRequest{TargetID: "local-postgres", SQL: "SELECT 1"}, "")
	wantCode(t, err, "access.denied")
	wantCode(t, s.CancelSession(id, "local-postgres", 1, false, ""), "access.denied")
	wantCode(t, s.CreateDatabase(id, "local-postgres", "smtest_db_x", "", ""), "access.denied")
	wantCode(t, s.CreateUser(id, "local-mysql", "smtest_db_x", "pw", "", "", ""), "access.denied")
	_, err = s.RedisCommand(id, "local-redis", 0, "GET x", false, "")
	wantCode(t, err, "access.denied")
	_, err = s.SaveTarget(id, TargetInput{Engine: EngineRedis, Mode: ModeLocal, Auth: AuthPeer}, "", false)
	wantCode(t, err, "access.denied")
	// reading status stays allowed
	must(s.Overview(id, "local-postgres", ""))(t)
}

func TestPGUnalignedFallback(t *testing.T) {
	s, c, id := setup(t)
	conn := must(c.Conn(id))(t)
	tg := must(s.target(id, "local-postgres"))(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := sqlReq{stmts: []string{"SELECT 1 AS a, NULL AS b, E'x\\ny' AS c, '' AS d", "SELECT 1 WHERE false", "SELECT 2 AS z"}, maxRows: 10, appName: "server-manager"}
	o, err := s.runPGFormat(ctx, conn, tg, r, "", false)
	if err != nil || !o.done || len(o.results) != 3 {
		t.Fatalf("%+v %v", o, err)
	}
	row := o.results[0].Rows[0]
	if *row[0] != "1" || row[1] != nil || *row[2] != "x\ny" || *row[3] != "" || len(o.results[0].Columns) != 4 {
		t.Fatalf("row %+v", o.results[0])
	}
	if !o.results[1].HasRows || len(o.results[1].Rows) != 0 || *o.results[2].Rows[0][0] != "2" {
		t.Fatalf("%+v", o.results)
	}
}

// TestDockerTargets runs database containers on the Docker test server and
// uses them through docker exec with the containers' own credentials.
func TestDockerTargets(t *testing.T) {
	c, id := testutil.Connect(t, testutil.DindPort())
	s := New(c)
	conn := must(c.Conn(id))(t)
	run := func(cmd string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		res, err := c.Run(ctx, conn, cmd, false, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return res.Stdout + res.Stderr
	}
	names := "smtest-db-pg smtest-db-my smtest-db-redis"
	run("docker rm -f " + names + " >/dev/null 2>&1")
	t.Cleanup(func() { run("docker rm -f " + names + " >/dev/null 2>&1") })
	for _, img := range []string{"postgres:16-alpine", "mariadb:11", "redis:7-alpine"} {
		if out := run("docker image inspect " + img + " >/dev/null 2>&1 || docker pull -q " + img + " 2>&1"); strings.Contains(out, "rror") {
			t.Skipf("cannot pull %s: %s", img, out)
		}
	}
	run("docker run -d --name smtest-db-pg -e POSTGRES_PASSWORD=pgsecret -e POSTGRES_USER=app postgres:16-alpine")
	run("docker run -d --name smtest-db-my -e MARIADB_ROOT_PASSWORD='my$ecret' mariadb:11")
	run("docker run -d --name smtest-db-redis redis:7-alpine")
	ready := `for i in $(seq 1 60); do
	  docker exec smtest-db-pg pg_isready -U app >/dev/null 2>&1 && docker exec smtest-db-my mariadb -uroot -p'my$ecret' -e 'SELECT 1' >/dev/null 2>&1 && break; sleep 1; done`
	run(ready)

	d := must(s.Detect(id, ""))(t)
	if d.Docker != "ok" {
		t.Fatalf("docker: %s", d.Docker)
	}
	want := map[string]string{"docker-smtest-db-pg": EnginePostgres, "docker-smtest-db-my": EngineMySQL, "docker-smtest-db-redis": EngineRedis}
	for tid, eng := range want {
		found := false
		for _, tg := range d.Targets {
			if tg.ID == tid && tg.Engine == eng && tg.Mode == ModeDocker {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s not detected: %+v", tid, d.Targets)
		}
	}
	var mu sync.Mutex
	var cmds []string
	traceHook = func(cmd string) { mu.Lock(); cmds = append(cmds, cmd); mu.Unlock() }
	t.Cleanup(func() { traceHook = nil })

	pg := must(s.Overview(id, "docker-smtest-db-pg", ""))(t)
	if !strings.HasPrefix(pg.Version, "16") {
		t.Fatalf("pg: %+v", pg)
	}
	my := must(s.Overview(id, "docker-smtest-db-my", ""))(t)
	if my.MySQL.Flavor != "mariadb" || !strings.HasPrefix(my.Version, "11") {
		t.Fatalf("mariadb: %+v", my)
	}
	rd := must(s.Overview(id, "docker-smtest-db-redis", ""))(t)
	if !strings.HasPrefix(rd.Version, "7") {
		t.Fatalf("redis: %+v", rd)
	}
	for tid, pgFlavor := range map[string]bool{"docker-smtest-db-pg": true, "docker-smtest-db-my": false} {
		res := must(s.RunQuery(id, QueryRequest{TargetID: tid, SQL: trickySQL(pgFlavor)}, ""))(t)
		if res.ErrorIndex != 0 {
			t.Fatalf("%s: %+v", tid, res)
		}
		checkTricky(t, res.Statements[0])
	}
	r := must(s.RedisCommand(id, "docker-smtest-db-redis", 0, "SET smtest-db-k 1", true, ""))(t)
	if r.Output != "OK" {
		t.Fatalf("redis set: %+v", r)
	}
	// password target in a container: the value travels by name (-e VAR)
	tg := must(s.SaveTarget(id, TargetInput{Engine: EnginePostgres, Name: "pg pw", Mode: ModeDocker, Container: "smtest-db-pg",
		Auth: AuthPassword, Host: "127.0.0.1", User: "app", Database: "app"}, "pgsecret", true))(t)
	t.Cleanup(func() { _ = s.DeleteTarget(id, tg.ID) })
	res := must(s.RunQuery(id, QueryRequest{TargetID: tg.ID, SQL: "SELECT current_user, current_database()"}, ""))(t)
	if res.ErrorIndex != 0 || *res.Statements[0].Rows[0][0] != "app" {
		t.Fatalf("pw target: %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, cmd := range cmds {
		if strings.Contains(cmd, "pgsecret") || strings.Contains(cmd, "my$ecret") {
			t.Fatalf("secret on command line: %s", cmd)
		}
	}
}
