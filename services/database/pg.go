package database

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
	"server-manager/internal/sshx"
)

// PostgreSQL status queries. They need PostgreSQL 10+ (pg_stat_activity
// backend_type, pg_current_wal_lsn); peer access runs them as the postgres
// superuser, password targets see what their role may see.

const pgOverviewSQL1 = `SELECT current_setting('server_version'), current_setting('server_version_num')::int,
 extract(epoch from now() - pg_postmaster_start_time())::bigint,
 to_char(pg_postmaster_start_time(), 'YYYY-MM-DD"T"HH24:MI:SSOF'),
 current_setting('max_connections')::int, pg_is_in_recovery(),
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'),
 (SELECT count(*) FROM pg_locks WHERE NOT granted),
 (SELECT coalesce(max(extract(epoch from now() - query_start)), 0) FROM pg_stat_activity
   WHERE state = 'active' AND backend_type = 'client backend' AND pid <> pg_backend_pid()),
 (SELECT count(*) FROM pg_stat_activity WHERE state LIKE 'idle in transaction%'),
 CASE WHEN pg_is_in_recovery() THEN coalesce(extract(epoch from now() - pg_last_xact_replay_timestamp()), -1) ELSE -1 END,
 (SELECT extversion FROM pg_extension WHERE extname = 'pg_stat_statements')`

const pgByStateSQL = `SELECT coalesce(state, 'unknown'), count(*) FROM pg_stat_activity
 WHERE backend_type = 'client backend' GROUP BY 1 ORDER BY 2 DESC`

const pgDatabasesSQL = `SELECT d.datname, pg_get_userbyid(d.datdba), pg_encoding_to_char(d.encoding), d.datcollate,
 CASE WHEN has_database_privilege(d.oid, 'CONNECT') THEN pg_database_size(d.oid) ELSE -1 END,
 d.datallowconn, coalesce(s.numbackends, 0), coalesce(s.blks_hit, 0), coalesce(s.blks_read, 0),
 coalesce(s.xact_commit, 0), coalesce(s.xact_rollback, 0), coalesce(s.deadlocks, 0)
 FROM pg_database d LEFT JOIN pg_stat_database s ON s.datid = d.oid
 WHERE NOT d.datistemplate ORDER BY 5 DESC, 1`

const pgReplicasSQL = `SELECT pid, coalesce(usename, ''), coalesce(application_name, ''), coalesce(client_addr::text, ''),
 coalesce(state, ''), coalesce(sync_state, ''),
 CASE WHEN pg_is_in_recovery() OR replay_lsn IS NULL THEN -1 ELSE pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::bigint END,
 coalesce(extract(epoch from replay_lag), 0)
 FROM pg_stat_replication ORDER BY 1`

const pgSettingsSQL = `SELECT name, current_setting(name) FROM pg_settings WHERE name IN
 ('shared_buffers','work_mem','maintenance_work_mem','effective_cache_size','max_wal_size','wal_level',
  'port','listen_addresses','data_directory','log_min_duration_statement','autovacuum','TimeZone',
  'shared_preload_libraries','password_encryption','max_worker_processes') ORDER BY name`

func (s *DatabaseService) pgOverview(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) (Overview, error) {
	rs, err := s.runSQL(ctx, conn, t, "", []string{pgOverviewSQL1, pgByStateSQL, pgDatabasesSQL, pgReplicasSQL, pgSettingsSQL}, sudoPW)
	if err != nil {
		return Overview{}, err
	}
	o := Overview{Engine: EnginePostgres}
	p := &PgOverview{ByState: []NameCount{}, Databases: []DatabaseInfo{}, Replicas: []PgReplica{}, Settings: []KV{}, CacheHitRatio: -1}
	if r := rowsOf(rs, 0); len(r) > 0 {
		row := r[0]
		o.Version = cell(row, 0)
		p.VersionNum = cellInt(row, 1)
		o.Uptime = cellInt(row, 2)
		p.StartTime = cell(row, 3)
		p.MaxConnections = cellInt(row, 4)
		p.InRecovery = cellBool(row, 5)
		p.Connections = cellInt(row, 6)
		p.LocksWaiting = cellInt(row, 7)
		p.LongestQuery = cellFloat(row, 8)
		p.IdleInTx = cellInt(row, 9)
		p.ReplayDelay = cellFloat(row, 10)
		p.StatStatements = row[11] != nil
	}
	for _, row := range rowsOf(rs, 1) {
		p.ByState = append(p.ByState, NameCount{Name: cell(row, 0), Count: cellInt(row, 1)})
	}
	var hit, read int64
	for _, row := range rowsOf(rs, 2) {
		d := pgDatabaseRow(row)
		p.Databases = append(p.Databases, d)
		if d.Size > 0 {
			p.TotalSize += d.Size
		}
		hit += cellInt(row, 7)
		read += cellInt(row, 8)
		p.XactCommit += cellInt(row, 9)
		p.XactRollback += cellInt(row, 10)
		p.Deadlocks += cellInt(row, 11)
	}
	if hit+read > 0 {
		p.CacheHitRatio = float64(hit) / float64(hit+read)
	}
	for _, row := range rowsOf(rs, 3) {
		p.Replicas = append(p.Replicas, PgReplica{Pid: cellInt(row, 0), User: cell(row, 1), App: cell(row, 2), Client: cell(row, 3),
			State: cell(row, 4), SyncState: cell(row, 5), LagBytes: cellInt(row, 6), ReplayLag: cellFloat(row, 7)})
	}
	for _, row := range rowsOf(rs, 4) {
		p.Settings = append(p.Settings, KV{Name: cell(row, 0), Value: cell(row, 1)})
	}
	o.Postgres = p
	return o, nil
}

func pgDatabaseRow(row []*string) DatabaseInfo {
	d := DatabaseInfo{Name: cell(row, 0), Owner: cell(row, 1), Encoding: cell(row, 2), Collation: cell(row, 3),
		Size: cellInt(row, 4), AllowConn: cellBool(row, 5), Connections: cellInt(row, 6), CacheHit: -1, Tables: -1}
	if h, r := cellInt(row, 7), cellInt(row, 8); h+r > 0 {
		d.CacheHit = float64(h) / float64(h+r)
	}
	d.System = d.Name == "postgres"
	return d
}

func (s *DatabaseService) pgDatabases(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) ([]DatabaseInfo, error) {
	rs, err := s.runSQL(ctx, conn, t, "", []string{pgDatabasesSQL}, sudoPW)
	if err != nil {
		return nil, err
	}
	out := []DatabaseInfo{}
	for _, row := range rowsOf(rs, 0) {
		out = append(out, pgDatabaseRow(row))
	}
	return out, nil
}

const pgTablesSQL = `SELECT n.nspname, c.relname,
 CASE c.relkind WHEN 'r' THEN 'table' WHEN 'm' THEN 'matview' WHEN 'p' THEN 'partitioned' ELSE c.relkind::text END,
 greatest(c.reltuples, 0)::bigint, pg_total_relation_size(c.oid), pg_relation_size(c.oid), pg_indexes_size(c.oid),
 coalesce(s.n_dead_tup, 0), coalesce(to_char(greatest(s.last_vacuum, s.last_autovacuum), 'YYYY-MM-DD HH24:MI'), ''),
 coalesce(to_char(greatest(s.last_analyze, s.last_autoanalyze), 'YYYY-MM-DD HH24:MI'), '')
 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 LEFT JOIN pg_stat_all_tables s ON s.relid = c.oid
 WHERE c.relkind IN ('r', 'm', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname !~ '^pg_toast'
 ORDER BY 5 DESC LIMIT 100`

func (s *DatabaseService) pgTables(ctx context.Context, conn *sshx.Conn, t Target, database, sudoPW string) ([]TableInfo, error) {
	rs, err := s.runSQL(ctx, conn, t, database, []string{pgTablesSQL}, sudoPW)
	if err != nil {
		return nil, err
	}
	out := []TableInfo{}
	for _, row := range rowsOf(rs, 0) {
		out = append(out, TableInfo{Schema: cell(row, 0), Name: cell(row, 1), Kind: cell(row, 2), Rows: cellInt(row, 3),
			TotalBytes: cellInt(row, 4), DataBytes: cellInt(row, 5), IndexBytes: cellInt(row, 6), DeadRows: cellInt(row, 7),
			LastVacuum: cell(row, 8), LastUpdate: cell(row, 9)})
	}
	return out, nil
}

const pgActivitySQL = `SELECT pid, coalesce(usename, ''), coalesce(datname, ''), coalesce(application_name, ''),
 coalesce(client_addr::text, CASE WHEN client_port = -1 THEN 'local' ELSE '' END), coalesce(state, ''),
 coalesce(wait_event_type || ': ' || wait_event, ''), coalesce(backend_type, ''),
 CASE WHEN state IS NOT NULL AND state <> 'idle' THEN coalesce(extract(epoch from now() - coalesce(query_start, state_change)), -1)
      WHEN state = 'idle' THEN coalesce(extract(epoch from now() - state_change), -1) ELSE -1 END,
 coalesce(extract(epoch from now() - xact_start), -1),
 coalesce(extract(epoch from now() - backend_start), -1),
 left(coalesce(query, ''), 4000), array_to_string(pg_blocking_pids(pid), ',')
 FROM pg_stat_activity WHERE pid <> pg_backend_pid()
 ORDER BY (state = 'active') DESC NULLS LAST, query_start NULLS LAST`

const pgLocksSQL = `SELECT l.pid, coalesce(a.usename, ''), coalesce(a.datname, ''), l.locktype, l.mode,
 coalesce(l.relation::regclass::text, ''), array_to_string(pg_blocking_pids(l.pid), ','),
 coalesce(extract(epoch from now() - a.query_start), 0), left(coalesce(a.query, ''), 1000)
 FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted ORDER BY 8 DESC`

func (s *DatabaseService) pgActivity(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) (Activity, error) {
	rs, err := s.runSQL(ctx, conn, t, "", []string{pgActivitySQL, pgLocksSQL, "SELECT current_setting('max_connections')::int"}, sudoPW)
	if err != nil {
		return Activity{}, err
	}
	a := Activity{Sessions: []Session{}, Locks: []LockWait{}}
	for _, row := range rowsOf(rs, 0) {
		a.Sessions = append(a.Sessions, Session{ID: cellInt(row, 0), User: cell(row, 1), Database: cell(row, 2), App: cell(row, 3),
			Client: cell(row, 4), State: cell(row, 5), Wait: cell(row, 6), BackendType: cell(row, 7), Duration: cellFloat(row, 8),
			XactAge: cellFloat(row, 9), Age: cellFloat(row, 10), Query: cell(row, 11), BlockedBy: intList(cell(row, 12))})
	}
	for _, row := range rowsOf(rs, 1) {
		a.Locks = append(a.Locks, LockWait{Pid: cellInt(row, 0), User: cell(row, 1), Database: cell(row, 2), LockType: cell(row, 3),
			Mode: cell(row, 4), Relation: cell(row, 5), BlockedBy: intList(cell(row, 6)), Waiting: cellFloat(row, 7), Query: cell(row, 8)})
	}
	if r := rowsOf(rs, 2); len(r) > 0 {
		a.MaxConnections = cellInt(r[0], 0)
	}
	return a, nil
}

func intList(s string) []int64 {
	out := []int64{}
	for _, f := range strings.Split(s, ",") {
		if v, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func (s *DatabaseService) pgSignal(ctx context.Context, conn *sshx.Conn, t Target, pid int64, terminate bool, sudoPW string) error {
	fn := "pg_cancel_backend"
	if terminate {
		fn = "pg_terminate_backend"
	}
	rs, err := s.runSQL(ctx, conn, t, "", []string{"SELECT " + fn + "(" + strconv.FormatInt(pid, 10) + ")"}, sudoPW)
	if err != nil {
		return err
	}
	if r := rowsOf(rs, 0); len(r) == 0 || !cellBool(r[0], 0) {
		return apperr.New("db.signalFailed", "pid", strconv.FormatInt(pid, 10))
	}
	return nil
}

func (s *DatabaseService) pgSlow(ctx context.Context, conn *sshx.Conn, t Target, database, order, sudoPW string) (SlowReport, error) {
	rs, err := s.runSQL(ctx, conn, t, database, []string{
		`SELECT current_setting('server_version_num')::int,
		 (SELECT extversion FROM pg_extension WHERE extname = 'pg_stat_statements'),
		 coalesce((SELECT setting FROM pg_settings WHERE name = 'shared_preload_libraries'), ''),
		 current_database()`,
	}, sudoPW)
	if err != nil {
		return SlowReport{}, err
	}
	rep := SlowReport{Source: "none", Queries: []SlowQuery{}}
	r := rowsOf(rs, 0)
	if len(r) == 0 {
		return rep, nil
	}
	ver := cellInt(r[0], 0)
	installed := r[0][1] != nil
	rep.Preloaded = strings.Contains(cell(r[0], 2), "pg_stat_statements")
	rep.Database = cell(r[0], 3)
	if !installed {
		rep.Reason = "notInstalled"
		if !rep.Preloaded {
			rep.Reason = "notPreloaded"
		}
		return rep, nil
	}
	col := map[string]string{"total": "total_exec_time", "mean": "mean_exec_time", "calls": "calls", "max": "max_exec_time"}[order]
	if col == "" {
		col = "total_exec_time"
	}
	tot, mean, mx := "total_exec_time", "mean_exec_time", "max_exec_time"
	if ver < 130000 {
		tot, mean, mx = "total_time", "mean_time", "max_time"
		col = strings.Replace(col, "_exec", "", 1)
	}
	q := `SELECT left(s.query, 3000), s.calls, s.` + tot + `, s.` + mean + `, s.` + mx + `, s.rows,
	 coalesce(d.datname, ''), coalesce(r.rolname, ''), s.shared_blks_hit, s.shared_blks_read
	 FROM pg_stat_statements s LEFT JOIN pg_database d ON d.oid = s.dbid LEFT JOIN pg_roles r ON r.oid = s.userid
	 ORDER BY s.` + col + ` DESC NULLS LAST LIMIT 50`
	rs, err = s.runSQL(ctx, conn, t, database, []string{q}, sudoPW)
	if err != nil {
		ae := apperr.From(err)
		rep.Reason = "error"
		rep.Detail = ae.Detail
		if strings.Contains(ae.Detail, "must be loaded via") || strings.Contains(ae.Detail, "shared_preload_libraries") {
			rep.Reason = "notPreloaded"
		}
		return rep, nil
	}
	rep.Source = "pg_stat_statements"
	for _, row := range rowsOf(rs, 0) {
		sq := SlowQuery{Query: cell(row, 0), Calls: cellInt(row, 1), TotalMs: cellFloat(row, 2), MeanMs: cellFloat(row, 3),
			MaxMs: cellFloat(row, 4), Rows: cellInt(row, 5), Database: cell(row, 6), User: cell(row, 7), HitRatio: -1}
		if h, rd := cellInt(row, 8), cellInt(row, 9); h+rd > 0 {
			sq.HitRatio = float64(h) / float64(h+rd)
		}
		rep.Queries = append(rep.Queries, sq)
	}
	return rep, nil
}

// pgQuoteIdent quotes a validated identifier.
func pgQuoteIdent(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }

// pgQuoteLiteral quotes a string (standard_conforming_strings is on).
func pgQuoteLiteral(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// scramVerifier computes a SCRAM-SHA-256 password verifier (like psql's
// \password) so the plain password never reaches the server's logs.
func scramVerifier(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := randRead(salt); err != nil {
		return "", err
	}
	const iter = 4096
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err != nil {
		return "", err
	}
	mac := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	clientKey := mac(salted, "Client Key")
	stored := sha256.Sum256(clientKey)
	serverKey := mac(salted, "Server Key")
	b64 := base64.StdEncoding.EncodeToString
	return "SCRAM-SHA-256$" + strconv.Itoa(iter) + ":" + b64(salt) + "$" + b64(stored[:]) + ":" + b64(serverKey), nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func pgCreateUserSQL(name, password string) (string, error) {
	lit := pgQuoteLiteral(password)
	if isASCII(password) {
		// Non-ASCII passwords go through the server's SASLprep instead.
		v, err := scramVerifier(password)
		if err != nil {
			return "", err
		}
		lit = pgQuoteLiteral(v)
	}
	return "CREATE ROLE " + pgQuoteIdent(name) + " LOGIN PASSWORD " + lit, nil
}
