package database

import (
	"reflect"
	"strings"
	"testing"

	"server-manager/internal/apperr"
)

func texts(st []stmt) []string {
	out := []string{}
	for _, s := range st {
		out = append(out, s.Text)
	}
	return out
}

func TestSplitSQLPostgres(t *testing.T) {
	src := `select 'a;b', "x;y", $$ ; $$, $f$ a $$ ; $f$; -- c;
/* multi ; /* nested ; */ still */ insert into t values (';'); create function f() returns int as $body$ begin; end $body$ language sql;
select E'it\'s; ok'; select 1::int ; ;`
	st, err := splitSQL(src, dialectPG)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`select 'a;b', "x;y", $$ ; $$, $f$ a $$ ; $f$`,
		"-- c;\n/* multi ; /* nested ; */ still */ insert into t values (';')",
		`create function f() returns int as $body$ begin; end $body$ language sql`,
		`select E'it\'s; ok'`,
		`select 1::int`,
	}
	if got := texts(st); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if st[1].first() != "INSERT" || st[3].first() != "SELECT" {
		t.Fatalf("first words: %q %q", st[1].first(), st[3].first())
	}
	// parentheses keep psql from splitting
	st, _ = splitSQL("select (1;2); select 3", dialectPG)
	if len(st) != 2 {
		t.Fatalf("paren split: %q", texts(st))
	}
}

func TestSplitSQLRejects(t *testing.T) {
	cases := map[string]string{
		`select 1; \! id`:     "db.metaCommand",
		`select 1 \g`:         "db.metaCommand",
		`\o | cat`:            "db.metaCommand",
		`select 'abc`:         "db.unterminated",
		`select $x$ abc`:      "db.unterminated",
		`select 1 /* x`:       "db.unterminated",
		`select 'a\' \! id '`: "db.metaCommand", // standard strings: \ is literal, string ends early
		`select "a`:           "db.unterminated",
		`select E'a\'' \! ls`: "db.metaCommand",
	}
	for src, code := range cases {
		_, err := splitSQL(src, dialectPG)
		if !apperr.HasCode(err, code) {
			t.Errorf("%q: got %v, want %s", src, err, code)
		}
	}
	for src, code := range map[string]string{
		"select 1;\nDELIMITER //": "db.delimiterUnsupported",
		`select 1 \G`:             "db.metaCommand",
		`select 'a\'b`:            "db.unterminated",
		"select 1; system ls":     "",
		`select "x\" \! ls"`:      "",
		"select 1 # c ;\n, 2":     "",
		"select 1 -- c;\n":        "",
		"select 1--2;":            "",
	} {
		_, err := splitSQL(src, dialectMySQL)
		if code == "" && err != nil || code != "" && !apperr.HasCode(err, code) {
			t.Errorf("mysql %q: got %v, want %q", src, err, code)
		}
	}
	st, _ := splitSQL("select 1--2; select `a;b` from t # x;\n; select 1 /*! INTO OUTFILE '/tmp/x' */", dialectMySQL)
	if len(st) != 3 || st[0].Text != "select 1--2" || !st[2].has("OUTFILE") {
		t.Fatalf("mysql split: %q", texts(st))
	}
}

func TestReadOnlyCheck(t *testing.T) {
	ok := []string{"select 1", "with x as (select 1) select * from x", "explain select 1", "show server_version", "values (1)"}
	for _, q := range ok {
		st, _ := splitSQL(q, dialectPG)
		if err := checkStatements(st, dialectPG, true); err != nil {
			t.Errorf("%q rejected: %v", q, err)
		}
	}
	bad := []string{"insert into t values (1)", "commit; delete from t", "set default_transaction_read_only = off",
		"select pg_terminate_backend(1)", `select "pg_terminate_backend"(1)`, "select query_to_xml('delete from t', true, true, '')",
		"copy t to program 'id'", "begin", "do $$ begin end $$"}
	for _, q := range bad {
		st, err := splitSQL(q, dialectPG)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkStatements(st, dialectPG, true); !apperr.HasCode(err, "db.readOnly") {
			t.Errorf("%q: %v", q, err)
		}
	}
	st, _ := splitSQL("copy t from stdin", dialectPG)
	if err := checkStatements(st, dialectPG, false); !apperr.HasCode(err, "db.copyUnsupported") {
		t.Errorf("copy stdin: %v", err)
	}
	for _, q := range []string{"select * from t into outfile '/tmp/x'", "update t set a=1", "set session transaction read write", "select 1 for update"} {
		st, _ := splitSQL(q, dialectMySQL)
		if err := checkStatements(st, dialectMySQL, true); !apperr.HasCode(err, "db.readOnly") {
			t.Errorf("mysql %q: %v", q, err)
		}
	}
	st, _ = splitSQL("load data local infile '/etc/passwd' into table t", dialectMySQL)
	if err := checkStatements(st, dialectMySQL, false); !apperr.HasCode(err, "db.localInfile") {
		t.Errorf("local infile: %v", err)
	}
}

func str(s string) *string { return &s }

func TestParsePGCSV(t *testing.T) {
	seg := "a,\"b,c\",c,d\n1,\"x\ty\r\nz\",NULLTOK,\n2,\"q\"\"q\",\"NULLTOK\",\"\"\n"
	has, cols, rows, total := parsePGCSV(seg, "NULLTOK", 10)
	if !has || total != 2 || !reflect.DeepEqual(cols, []string{"a", "b,c", "c", "d"}) {
		t.Fatalf("got %v %v %d", has, cols, total)
	}
	want := [][]*string{{str("1"), str("x\ty\r\nz"), nil, str("")}, {str("2"), str(`q"q`), str("NULLTOK"), str("")}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %v", rows)
	}
	if has, _, _, _ := parsePGCSV("", "N", 10); has {
		t.Fatal("empty segment has rows")
	}
	has, cols, rows, total = parsePGCSV("x\n", "N", 10)
	if !has || len(cols) != 1 || len(rows) != 0 || total != 0 {
		t.Fatal("header only")
	}
	_, _, rows, total = parsePGCSV("x\n1\n2\n3\n", "N", 2)
	if len(rows) != 2 || total != 3 {
		t.Fatal("row cap")
	}
}

func TestParseMyXML(t *testing.T) {
	out := `<?xml version="1.0"?>

<resultset statement="select 1 as a, 'x' as ` + "`b&lt;c&gt;`" + `" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance">
  <row>
	<field name="a">1</field>
	<field name="b&lt;c&gt;">x	y
z &amp; &lt;/field&gt; &quot;q&quot;</field>
	<field name="c" xsi:nil="true" />
	<field name="d"></field>
  </row>
</resultset>
<?xml version="1.0"?>

<resultset statement="select 1 from dual where false" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"></resultset>
`
	sets := parseMyXML(out, 10)
	if len(sets) != 2 {
		t.Fatalf("%d sets", len(sets))
	}
	if !reflect.DeepEqual(sets[0].cols, []string{"a", "b<c>", "c", "d"}) {
		t.Fatalf("cols %q", sets[0].cols)
	}
	want := []*string{str("1"), str("x\ty\nz & </field> \"q\""), nil, str("")}
	if !reflect.DeepEqual(sets[0].rows[0], want) {
		t.Fatalf("row %v", sets[0].rows[0])
	}
	if sets[1].total != 0 || len(sets[1].rows) != 0 {
		t.Fatal("empty set")
	}
}

func TestRedisTTY(t *testing.T) {
	out := strings.Join([]string{
		"1) 1) (integer) 14",
		"   2) (integer) 1790307937",
		"   3) (integer) 1926",
		"   4) 1) \"COMMAND\"",
		"      2) \"DOCS\"",
		"   5) \"127.0.0.1:45190\"",
		"   6) \"\"",
		"2) 1) (integer) 13",
		"   2) (integer) 1790307937",
		"   3) (integer) 3",
		"   4) 1) \"set\"",
		"      2) \"a\\x00\\\"b\\n\"",
		"   5) \"127.0.0.1:45180\"",
		"   6) \"\"",
	}, "\n")
	n := parseTTY(out)
	if n.kind != "arr" || len(n.arr) != 2 || len(n.arr[0].arr) != 6 || n.arr[1].arr[3].arr[1].s != "a\x00\"b\n" {
		t.Fatalf("%+v", n)
	}
	// 10+ elements: right-aligned indexes
	var lines []string
	for i := 1; i <= 11; i++ {
		lines = append(lines, strings.Repeat(" ", len("11")-len(itoa(i)))+itoa(i)+") \"v"+itoa(i)+"\"")
	}
	n = parseTTY(strings.Join(lines, "\n"))
	if len(n.arr) != 11 || n.arr[10].s != "v11" {
		t.Fatalf("padded: %+v", n)
	}
	if n := parseTTY("(error) ERR unknown command"); n.kind != "err" {
		t.Fatal(n)
	}
	if n := parseTTY("(empty array)"); n.kind != "arr" || len(n.arr) != 0 {
		t.Fatal(n)
	}
}

func TestRedisArgs(t *testing.T) {
	args, err := splitRedisCommand(`set "a b" 'it\'s' "x\x00y\n" plain`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"set", "a b", "it's", "x\x00y\n", "plain"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("%q", args)
	}
	for _, bad := range []string{`get "abc`, `get 'x`, `get "a"b`} {
		if _, err := splitRedisCommand(bad); !apperr.HasCode(err, "db.redis.badQuote") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, a := range []string{"x\x00\"\\\n\xffé", ""} {
		if got := unrepr(encodeRedisArg(a)); got != a {
			t.Errorf("round trip %q → %q", a, got)
		}
	}
	for cmd, cls := range map[string]string{"GET k": "read", "flushall": "write", "config get *": "read", "CONFIG SET x y": "write",
		"monitor": "blocked", "client reply off": "blocked", "slowlog reset": "write", "acl log": "read", "acl log reset": "write", "keys *": "read"} {
		a, _ := splitRedisCommand(cmd)
		if got := redisClass(a); got != cls {
			t.Errorf("%s: %s want %s", cmd, got, cls)
		}
	}
	a, _ := splitRedisCommand("config set requirepass s3cret")
	if strings.Contains(redactRedis(a), "s3cret") {
		t.Fatal("not redacted")
	}
}

func TestInfoParse(t *testing.T) {
	info := "# Server\r\nredis_version:7.0.15\r\nuptime_in_seconds:100\r\n\r\n# Stats\r\nkeyspace_hits:3\r\nkeyspace_misses:1\r\n# Keyspace\r\ndb0:keys=5,expires=1,avg_ttl=10\r\ndb2:keys=1,expires=0,avg_ttl=0\r\n"
	o, err := redisOverviewFrom(info, parseTTY("(empty array)"), parseTTY("1) \"maxmemory-policy\"\n2) \"noeviction\""), parseTTY("1) \"maxmemory\"\n2) \"0\""))
	if err != nil {
		t.Fatal(err)
	}
	r := o.Redis
	if o.Version != "7.0.15" || o.Uptime != 100 || r.HitRatio != 0.75 || len(r.Keyspace) != 2 || r.Keyspace[1].DB != 2 || r.MaxMemoryPolicy != "noeviction" {
		t.Fatalf("%+v %+v", o, r)
	}
}

func TestDetectParse(t *testing.T) {
	out := `@@bin psql /usr/bin/psql
@@bin mysql /usr/bin/mysql
@@bin mariadb /usr/bin/mariadb
@@ver psql psql (PostgreSQL) 15.19 (Debian 15.19-0+deb12u1)
@@ver mysql mariadb  Ver 15.1 Distrib 10.11.18-MariaDB, for debian-linux-gnu (aarch64) using  EditLine wrapper
@@ver postgres postgres (PostgreSQL) 15.19 (Debian 15.19-0+deb12u1)
@@ver mysqld /usr/sbin/mariadbd  Ver 10.11.18-MariaDB-0+deb12u1 for debian-linux-gnu on aarch64 (Debian 12)
@@unit mariadb.service loaded active running MariaDB 10.11.18 database server
@@unit postgresql.service loaded active exited PostgreSQL RDBMS
@@unit postgresql@15-main.service loaded active running PostgreSQL Cluster 15-main
@@listen 127.0.0.1:6379
@@listen [::1]:5432
@@docker ok
@@ctr abc	pg16	postgres:16-alpine	Up 2 hours
@@ctr def	web	nginx:latest	Up 2 hours
@@ctr ghi	cache	docker.io/library/redis:7	Up 1 hour
@@end`
	d := parseDetect(out)
	if len(d.Targets) != 5 {
		t.Fatalf("targets %+v", d.Targets)
	}
	if d.Targets[1].Name != "MariaDB" || d.Engines[0].ServerVersion != "15.19" || d.Engines[1].ServerVersion != "10.11.18" || !d.Engines[2].Running {
		t.Fatalf("%+v", d)
	}
	if d.Targets[3].ID != "docker-pg16" || d.Targets[3].Engine != EnginePostgres || d.Targets[4].Engine != EngineRedis || d.Containers != 2 {
		t.Fatalf("%+v", d.Targets)
	}
	if len(d.Engines[0].Units) != 1 {
		t.Fatalf("umbrella unit kept: %+v", d.Engines[0].Units)
	}
}

func TestScram(t *testing.T) {
	v, err := scramVerifier("secret")
	if err != nil || !strings.HasPrefix(v, "SCRAM-SHA-256$4096:") || strings.Count(v, "$") != 2 {
		t.Fatalf("%q %v", v, err)
	}
	if q, _ := pgCreateUserSQL("u", "pw'x"); strings.Contains(q, "pw'x") {
		t.Fatal("plain password in SQL")
	}
	if myQuoteLiteral(`a'b\c`) != `'a\'b\\c'` {
		t.Fatal(myQuoteLiteral(`a'b\c`))
	}
}

// A MySQL container publishing 3306 must not also appear as a host MySQL.
func TestDetectDockerOnly(t *testing.T) {
	out := "@@bin docker /usr/bin/docker\n@@listen 0.0.0.0:3306\n@@listen [::]:3306\n@@proc mysqld\n@@docker ok\n" +
		"@@ctr abc123\tcity-family-test-mysql-1\tmysql:8.0\tUp 2 hours\t0.0.0.0:3306->3306/tcp, [::]:3306->3306/tcp\n@@end\n"
	d := parseDetect(out)
	for _, tg := range d.Targets {
		if tg.Mode == ModeLocal {
			t.Fatalf("unexpected host target %+v", tg)
		}
	}
	if len(d.Targets) != 1 || d.Targets[0].Container != "city-family-test-mysql-1" {
		t.Fatalf("targets %+v", d.Targets)
	}
	// With a host mysqld binary both are listed.
	d = parseDetect("@@ver mysqld /usr/sbin/mysqld  Ver 8.0.36\n" + out)
	if len(d.Targets) != 2 {
		t.Fatalf("want host + docker, got %+v", d.Targets)
	}
}
