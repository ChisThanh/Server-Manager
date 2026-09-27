package database

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// MySQL / MariaDB status queries.

var mySystemDBs = map[string]bool{"mysql": true, "information_schema": true, "performance_schema": true, "sys": true}

const myStatusSQL = `SHOW GLOBAL STATUS WHERE Variable_name IN ('Uptime','Threads_connected','Threads_running',
 'Max_used_connections','Questions','Slow_queries','Connections','Aborted_connects',
 'Innodb_buffer_pool_read_requests','Innodb_buffer_pool_reads','Innodb_buffer_pool_pages_total',
 'Innodb_buffer_pool_pages_free')`

const myVarsSQL = `SHOW GLOBAL VARIABLES WHERE Variable_name IN ('slow_query_log','long_query_time','slow_query_log_file',
 'log_output','log_queries_not_using_indexes','innodb_buffer_pool_size','max_connections','datadir','port',
 'bind_address','character_set_server','collation_server','wait_timeout','performance_schema','log_bin',
 'server_id','max_allowed_packet','innodb_log_file_size','table_open_cache','thread_cache_size','version_comment')`

// settings shown in the overview (the rest feed computed fields)
var myShownSettings = []string{"bind_address", "port", "datadir", "character_set_server", "collation_server",
	"innodb_buffer_pool_size", "innodb_log_file_size", "max_allowed_packet", "wait_timeout", "table_open_cache",
	"thread_cache_size", "performance_schema", "log_bin", "server_id"}

func kvMap(rows [][]*string) map[string]string {
	m := map[string]string{}
	for _, r := range rows {
		m[strings.ToLower(cell(r, 0))] = cell(r, 1)
	}
	return m
}

func atoi64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

func onOff(s string) bool {
	s = strings.ToUpper(strings.TrimSpace(s))
	return s == "ON" || s == "1" || s == "TRUE" || s == "YES"
}

func slowLogFrom(v map[string]string) SlowLogSettings {
	lqt, _ := strconv.ParseFloat(v["long_query_time"], 64)
	return SlowLogSettings{Enabled: onOff(v["slow_query_log"]), LongQueryTime: lqt, File: v["slow_query_log_file"],
		Output: v["log_output"], NotUsingIndexes: onOff(v["log_queries_not_using_indexes"])}
}

var replicaFields = []string{"Replica_IO_Running", "Slave_IO_Running", "Replica_SQL_Running", "Slave_SQL_Running",
	"Seconds_Behind_Source", "Seconds_Behind_Master", "Source_Host", "Master_Host", "Source_Port", "Master_Port",
	"Last_IO_Error", "Last_SQL_Error", "Last_Error", "Relay_Log_Space", "Using_Gtid", "Gtid_IO_Pos", "Executed_Gtid_Set"}

func (s *DatabaseService) myOverview(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) (Overview, error) {
	stmts := []string{
		"SELECT VERSION(), @@version_comment, (SELECT COUNT(*) FROM information_schema.SCHEMATA)",
		myStatusSQL, myVarsSQL, "SHOW REPLICA STATUS",
	}
	rs, err := s.runSQL(ctx, conn, t, "", stmts, sudoPW)
	if err != nil && len(rs) < 3 {
		return Overview{}, err
	}
	var repl [][]*string
	var replCols []string
	if err == nil && len(rs) > 3 {
		repl, replCols = rs[3].Rows, rs[3].Columns
	} else if r2, err2 := s.runSQL(ctx, conn, t, "", []string{"SHOW SLAVE STATUS"}, sudoPW); err2 == nil && len(r2) > 0 {
		repl, replCols = r2[0].Rows, r2[0].Columns
	}
	o := Overview{Engine: EngineMySQL}
	m := &MyOverview{Replication: []KV{}, Settings: []KV{}, BufferPoolHitRatio: -1}
	if r := rowsOf(rs, 0); len(r) > 0 {
		o.Version = cell(r[0], 0)
		m.VersionComment = cell(r[0], 1)
		m.Databases = cellInt(r[0], 2)
	}
	m.Flavor = "mysql"
	if strings.Contains(strings.ToLower(o.Version+" "+m.VersionComment), "mariadb") {
		m.Flavor = "mariadb"
	}
	st := kvMap(rowsOf(rs, 1))
	vars := kvMap(rowsOf(rs, 2))
	o.Uptime = atoi64(st["uptime"])
	m.ThreadsConnected = atoi64(st["threads_connected"])
	m.ThreadsRunning = atoi64(st["threads_running"])
	m.MaxUsedConnections = atoi64(st["max_used_connections"])
	m.Questions = atoi64(st["questions"])
	m.SlowQueries = atoi64(st["slow_queries"])
	m.Connections = atoi64(st["connections"])
	m.AbortedConnects = atoi64(st["aborted_connects"])
	if req := atoi64(st["innodb_buffer_pool_read_requests"]); req > 0 {
		m.BufferPoolHitRatio = 1 - float64(atoi64(st["innodb_buffer_pool_reads"]))/float64(req)
	}
	if tot := atoi64(st["innodb_buffer_pool_pages_total"]); tot > 0 {
		m.BufferPoolUsed = 1 - float64(atoi64(st["innodb_buffer_pool_pages_free"]))/float64(tot)
	}
	m.MaxConnections = atoi64(vars["max_connections"])
	m.BufferPoolSize = atoi64(vars["innodb_buffer_pool_size"])
	m.SlowLog = slowLogFrom(vars)
	for _, k := range myShownSettings {
		if v, ok := vars[k]; ok {
			m.Settings = append(m.Settings, KV{Name: k, Value: v})
		}
	}
	if len(repl) > 0 {
		idx := map[string]int{}
		for i, c := range replCols {
			idx[c] = i
		}
		for _, f := range replicaFields {
			if i, ok := idx[f]; ok {
				m.Replication = append(m.Replication, KV{Name: f, Value: cell(repl[0], i)})
			}
		}
	}
	o.MySQL = m
	return o, nil
}

const myDatabasesSQL = `SELECT s.SCHEMA_NAME, s.DEFAULT_CHARACTER_SET_NAME, s.DEFAULT_COLLATION_NAME,
 COALESCE(SUM(t.DATA_LENGTH + t.INDEX_LENGTH), 0), COUNT(t.TABLE_NAME)
 FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.TABLE_SCHEMA = s.SCHEMA_NAME
 GROUP BY s.SCHEMA_NAME, s.DEFAULT_CHARACTER_SET_NAME, s.DEFAULT_COLLATION_NAME ORDER BY 4 DESC, 1`

func (s *DatabaseService) myDatabases(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) ([]DatabaseInfo, error) {
	rs, err := s.runSQL(ctx, conn, t, "", []string{myDatabasesSQL}, sudoPW)
	if err != nil {
		return nil, err
	}
	out := []DatabaseInfo{}
	for _, row := range rowsOf(rs, 0) {
		name := cell(row, 0)
		out = append(out, DatabaseInfo{Name: name, Encoding: cell(row, 1), Collation: cell(row, 2), Size: cellInt(row, 3),
			Tables: cellInt(row, 4), Connections: -1, AllowConn: true, CacheHit: -1, System: mySystemDBs[strings.ToLower(name)]})
	}
	return out, nil
}

func (s *DatabaseService) myTables(ctx context.Context, conn *sshx.Conn, t Target, database, sudoPW string) ([]TableInfo, error) {
	where := "TABLE_SCHEMA NOT IN ('mysql','information_schema','performance_schema','sys')"
	if database != "" {
		where = "TABLE_SCHEMA = '" + database + "'" // validated: [A-Za-z0-9_$-]
	}
	q := `SELECT TABLE_SCHEMA, TABLE_NAME, COALESCE(ENGINE, TABLE_TYPE), COALESCE(TABLE_ROWS, 0),
	 COALESCE(DATA_LENGTH, 0) + COALESCE(INDEX_LENGTH, 0), COALESCE(DATA_LENGTH, 0), COALESCE(INDEX_LENGTH, 0),
	 COALESCE(DATA_FREE, 0), COALESCE(DATE_FORMAT(UPDATE_TIME, '%Y-%m-%d %H:%i'), '')
	 FROM information_schema.TABLES WHERE TABLE_TYPE = 'BASE TABLE' AND ` + where + ` ORDER BY 5 DESC LIMIT 100`
	rs, err := s.runSQL(ctx, conn, t, "", []string{q}, sudoPW)
	if err != nil {
		return nil, err
	}
	out := []TableInfo{}
	for _, row := range rowsOf(rs, 0) {
		out = append(out, TableInfo{Schema: cell(row, 0), Name: cell(row, 1), Kind: cell(row, 2), Rows: cellInt(row, 3),
			TotalBytes: cellInt(row, 4), DataBytes: cellInt(row, 5), IndexBytes: cellInt(row, 6), FreeBytes: cellInt(row, 7),
			LastUpdate: cell(row, 8)})
	}
	return out, nil
}

const myProcessSQL = `SELECT ID, COALESCE(USER, ''), COALESCE(HOST, ''), COALESCE(DB, ''), COALESCE(COMMAND, ''),
 COALESCE(TIME, 0), COALESCE(STATE, ''), LEFT(COALESCE(INFO, ''), 4000)
 FROM information_schema.PROCESSLIST WHERE ID <> CONNECTION_ID() ORDER BY TIME DESC`

func (s *DatabaseService) myActivity(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) (Activity, error) {
	rs, err := s.runSQL(ctx, conn, t, "", []string{myProcessSQL, "SELECT @@max_connections"}, sudoPW)
	if err != nil {
		return Activity{}, err
	}
	a := Activity{Sessions: []Session{}, Locks: []LockWait{}}
	for _, row := range rowsOf(rs, 0) {
		cmd := cell(row, 4)
		state := cell(row, 6)
		switch {
		case cmd == "Sleep":
			state = "idle"
		case cmd == "Daemon" || cmd == "Binlog Dump" || cmd == "Binlog Dump GTID" || cmd == "Slave_IO" || cmd == "Slave_SQL":
			if state == "" {
				state = "background"
			}
		case state == "":
			state = "active"
		}
		bt := "client"
		if cell(row, 1) == "system user" || cell(row, 1) == "event_scheduler" || cmd == "Daemon" {
			bt = "background"
		}
		a.Sessions = append(a.Sessions, Session{ID: cellInt(row, 0), User: cell(row, 1), Client: cell(row, 2), Database: cell(row, 3),
			Command: cmd, Duration: cellFloat(row, 5), State: state, Query: cell(row, 7), XactAge: -1, Age: -1, BackendType: bt, BlockedBy: []int64{}})
	}
	if r := rowsOf(rs, 1); len(r) > 0 {
		a.MaxConnections = cellInt(r[0], 0)
	}
	return a, nil
}

func (s *DatabaseService) myKill(ctx context.Context, conn *sshx.Conn, t Target, id int64, connection bool, sudoPW string) error {
	q := "KILL QUERY " + strconv.FormatInt(id, 10)
	if connection {
		q = "KILL CONNECTION " + strconv.FormatInt(id, 10)
	}
	_, err := s.runSQL(ctx, conn, t, "", []string{q}, sudoPW)
	return err
}

func (s *DatabaseService) mySlow(ctx context.Context, conn *sshx.Conn, t Target, order, sudoPW string) (SlowReport, error) {
	rs, err := s.runSQL(ctx, conn, t, "", []string{myVarsSQL}, sudoPW)
	if err != nil {
		return SlowReport{}, err
	}
	vars := kvMap(rowsOf(rs, 0))
	rep := SlowReport{Source: "none", Queries: []SlowQuery{}, SlowLog: slowLogFrom(vars)}
	if !onOff(vars["performance_schema"]) {
		rep.Reason = "psDisabled"
		return rep, nil
	}
	col := map[string]string{"total": "SUM_TIMER_WAIT", "mean": "AVG_TIMER_WAIT", "calls": "COUNT_STAR", "max": "MAX_TIMER_WAIT"}[order]
	if col == "" {
		col = "SUM_TIMER_WAIT"
	}
	q := `SELECT LEFT(COALESCE(DIGEST_TEXT, ''), 3000), COUNT_STAR, SUM_TIMER_WAIT / 1000000000, AVG_TIMER_WAIT / 1000000000,
	 MAX_TIMER_WAIT / 1000000000, SUM_ROWS_SENT, COALESCE(SCHEMA_NAME, '')
	 FROM performance_schema.events_statements_summary_by_digest WHERE DIGEST_TEXT IS NOT NULL
	 ORDER BY ` + col + ` DESC LIMIT 50`
	rs, err = s.runSQL(ctx, conn, t, "", []string{q}, sudoPW)
	if err != nil {
		rep.Reason = "error"
		rep.Detail = apperr.From(err).Detail
		return rep, nil
	}
	rep.Source = "performance_schema"
	for _, row := range rowsOf(rs, 0) {
		rep.Queries = append(rep.Queries, SlowQuery{Query: cell(row, 0), Calls: cellInt(row, 1), TotalMs: cellFloat(row, 2),
			MeanMs: cellFloat(row, 3), MaxMs: cellFloat(row, 4), Rows: cellInt(row, 5), Database: cell(row, 6), HitRatio: -1})
	}
	return rep, nil
}

var reLogPath = regexp.MustCompile(`^/[A-Za-z0-9_./@+-]{1,250}$`)

// mySlowLogTail returns the last lines of the slow query log file.
func (s *DatabaseService) mySlowLogTail(ctx context.Context, conn *sshx.Conn, t Target, lines int, sudoPW string) (string, error) {
	rs, err := s.runSQL(ctx, conn, t, "", []string{"SELECT @@slow_query_log_file, @@datadir"}, sudoPW)
	if err != nil {
		return "", err
	}
	r := rowsOf(rs, 0)
	if len(r) == 0 {
		return "", apperr.New("db.slowLogMissing")
	}
	file := cell(r[0], 0)
	if file != "" && !strings.HasPrefix(file, "/") {
		file = strings.TrimSuffix(cell(r[0], 1), "/") + "/" + file
	}
	if !reLogPath.MatchString(file) || strings.Contains(file, "..") {
		return "", apperr.New("db.slowLogMissing").WithDetail(file)
	}
	cmd := "tail -n " + strconv.Itoa(lines) + " " + core.Q(file)
	var res sshx.ExecResult
	if t.Mode == ModeDocker {
		res, err = s.core.Run(ctx, conn, "docker exec "+core.Q(t.Container)+" "+cmd, s.dockerNeedsSudo(ctx, conn), sudoPW, "")
	} else {
		res, err = s.core.RunAuto(ctx, conn, cmd, sudoPW, "")
	}
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", apperr.New("db.slowLogMissing").WithDetail(firstLines(res.Stderr, 3))
	}
	return res.Stdout, nil
}

// myQuoteLiteral quotes a string for MySQL; the statement runs with
// NO_BACKSLASH_ESCAPES removed from sql_mode (see mySQLModePrelude).
func myQuoteLiteral(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)
	return "'" + r.Replace(v) + "'"
}

const mySQLModePrelude = `SET SESSION sql_mode = TRIM(BOTH ',' FROM REPLACE(CONCAT(',', @@SESSION.sql_mode, ','), ',NO_BACKSLASH_ESCAPES,', ','))`

func myQuoteIdent(n string) string { return "`" + strings.ReplaceAll(n, "`", "``") + "`" }
