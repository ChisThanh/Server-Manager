package database

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
)

// Exported methods (bindings). Reading status needs no permission; every
// query, command or change requires PermDatabase and is audited.

const statusTimeout = 45 * time.Second

func unsupported(t Target, op string) error {
	return apperr.New("db.unsupported", "engine", engineLabel(t.Engine), "op", op)
}

// Overview returns the status summary of a target.
func (s *DatabaseService) Overview(connID, targetID, sudoPassword string) (Overview, error) {
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return Overview{}, err
	}
	ctx, cancel := core.Timeout(statusTimeout)
	defer cancel()
	switch t.Engine {
	case EnginePostgres:
		return s.pgOverview(ctx, conn, t, sudoPassword)
	case EngineMySQL:
		return s.myOverview(ctx, conn, t, sudoPassword)
	default:
		return s.redisOverview(ctx, conn, t, sudoPassword)
	}
}

// Ping checks that the target is reachable with its settings; returns the
// server version.
func (s *DatabaseService) Ping(connID, targetID, sudoPassword string) (string, error) {
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return "", err
	}
	ctx, cancel := core.Timeout(30 * time.Second)
	defer cancel()
	if t.Engine == EngineRedis {
		p, err := s.runRedis(ctx, conn, t, 0, [][]string{{"INFO", "server"}}, sudoPassword)
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(p[0], "(error)") {
			return "", apperr.New("db.redis.failed").WithDetail(strings.TrimPrefix(p[0], "(error) "))
		}
		m := infoMap(parseInfo(p[0]))
		return core.FirstNonEmpty(m["valkey_version"], m["redis_version"]), nil
	}
	rs, err := s.runSQL(ctx, conn, t, "", []string{"SELECT version()"}, sudoPassword)
	if err != nil {
		return "", err
	}
	if r := rowsOf(rs, 0); len(r) > 0 {
		return cell(r[0], 0), nil
	}
	return "", nil
}

// Databases lists the databases of a Postgres or MySQL target with sizes.
func (s *DatabaseService) Databases(connID, targetID, sudoPassword string) ([]DatabaseInfo, error) {
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := core.Timeout(statusTimeout)
	defer cancel()
	switch t.Engine {
	case EnginePostgres:
		return s.pgDatabases(ctx, conn, t, sudoPassword)
	case EngineMySQL:
		return s.myDatabases(ctx, conn, t, sudoPassword)
	}
	return nil, unsupported(t, "databases")
}

// Tables lists the largest tables of a database ("" = all user databases
// on MySQL, the default database on Postgres).
func (s *DatabaseService) Tables(connID, targetID, database, sudoPassword string) ([]TableInfo, error) {
	if database != "" {
		if err := checkDBName(database); err != nil {
			return nil, err
		}
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := core.Timeout(statusTimeout)
	defer cancel()
	switch t.Engine {
	case EnginePostgres:
		return s.pgTables(ctx, conn, t, database, sudoPassword)
	case EngineMySQL:
		return s.myTables(ctx, conn, t, database, sudoPassword)
	}
	return nil, unsupported(t, "tables")
}

// Activity lists sessions (pg_stat_activity / processlist) and lock waits.
func (s *DatabaseService) Activity(connID, targetID, sudoPassword string) (Activity, error) {
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return Activity{}, err
	}
	ctx, cancel := core.Timeout(statusTimeout)
	defer cancel()
	switch t.Engine {
	case EnginePostgres:
		return s.pgActivity(ctx, conn, t, sudoPassword)
	case EngineMySQL:
		return s.myActivity(ctx, conn, t, sudoPassword)
	}
	return Activity{}, unsupported(t, "activity")
}

// CancelSession cancels the running query of a session (pg_cancel_backend,
// KILL QUERY) or, with terminate, closes the session (pg_terminate_backend,
// KILL CONNECTION).
func (s *DatabaseService) CancelSession(connID, targetID string, id int64, terminate bool, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return err
	}
	action := "db.session.cancel"
	if terminate {
		action = "db.session.terminate"
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err == nil && (id <= 0 || id > 1<<40) {
		err = apperr.New("db.invalidPid", "pid", strconv.FormatInt(id, 10))
	}
	if err == nil {
		ctx, cancel := core.Timeout(30 * time.Second)
		defer cancel()
		switch t.Engine {
		case EnginePostgres:
			err = s.pgSignal(ctx, conn, t, id, terminate, sudoPassword)
		case EngineMySQL:
			err = s.myKill(ctx, conn, t, id, terminate, sudoPassword)
		default:
			err = unsupported(t, "cancel")
		}
	}
	s.core.Audit(connID, action, targetLabel(t, targetID), "id "+strconv.FormatInt(id, 10), err)
	return err
}

// SlowQueries returns the most expensive statements: pg_stat_statements
// (Postgres, in database or the default one) or performance_schema digests
// (MySQL), ordered by total | mean | calls | max time.
func (s *DatabaseService) SlowQueries(connID, targetID, database, order, sudoPassword string) (SlowReport, error) {
	if database != "" {
		if err := checkDBName(database); err != nil {
			return SlowReport{}, err
		}
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return SlowReport{}, err
	}
	ctx, cancel := core.Timeout(statusTimeout)
	defer cancel()
	switch t.Engine {
	case EnginePostgres:
		return s.pgSlow(ctx, conn, t, database, order, sudoPassword)
	case EngineMySQL:
		return s.mySlow(ctx, conn, t, order, sudoPassword)
	}
	return SlowReport{}, unsupported(t, "slow")
}

// EnableStatStatements runs CREATE EXTENSION pg_stat_statements in a
// database (the library must already be in shared_preload_libraries).
func (s *DatabaseService) EnableStatStatements(connID, targetID, database, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return err
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err == nil && database != "" {
		err = checkDBName(database)
	}
	if err == nil && t.Engine != EnginePostgres {
		err = unsupported(t, "pg_stat_statements")
	}
	if err == nil {
		ctx, cancel := core.Timeout(60 * time.Second)
		defer cancel()
		_, err = s.runSQL(ctx, conn, t, database, []string{"CREATE EXTENSION IF NOT EXISTS pg_stat_statements"}, sudoPassword)
	}
	s.core.Audit(connID, "db.pgss.enable", targetLabel(t, targetID)+"/"+database, "", err)
	return err
}

// ResetSlowStats clears pg_stat_statements / performance_schema digests.
func (s *DatabaseService) ResetSlowStats(connID, targetID, database, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return err
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err == nil && database != "" {
		err = checkDBName(database)
	}
	if err == nil {
		ctx, cancel := core.Timeout(30 * time.Second)
		defer cancel()
		switch t.Engine {
		case EnginePostgres:
			_, err = s.runSQL(ctx, conn, t, database, []string{"SELECT pg_stat_statements_reset()"}, sudoPassword)
		case EngineMySQL:
			_, err = s.runSQL(ctx, conn, t, "", []string{"TRUNCATE TABLE performance_schema.events_statements_summary_by_digest"}, sudoPassword)
		default:
			err = unsupported(t, "slow")
		}
	}
	s.core.Audit(connID, "db.slow.reset", targetLabel(t, targetID), "", err)
	return err
}

// SetSlowLog turns the MySQL slow query log on/off and sets long_query_time
// (seconds, 0–3600) at runtime (SET GLOBAL; not persisted in my.cnf).
func (s *DatabaseService) SetSlowLog(connID, targetID string, enabled bool, longQueryTime float64, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return err
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err == nil && t.Engine != EngineMySQL {
		err = unsupported(t, "slowlog")
	}
	if err == nil && (longQueryTime < 0 || longQueryTime > 3600) {
		err = apperr.New("db.invalidValue", "value", fmt.Sprint(longQueryTime))
	}
	on := "OFF"
	if enabled {
		on = "ON"
	}
	if err == nil {
		ctx, cancel := core.Timeout(30 * time.Second)
		defer cancel()
		_, err = s.runSQL(ctx, conn, t, "", []string{"SET GLOBAL slow_query_log = " + on,
			"SET GLOBAL long_query_time = " + strconv.FormatFloat(longQueryTime, 'f', 3, 64)}, sudoPassword)
	}
	s.core.Audit(connID, "db.slowlog.set", targetLabel(t, targetID), fmt.Sprintf("slow_query_log=%s long_query_time=%g", on, longQueryTime), err)
	return err
}

// SlowLogTail returns the last lines (10–5000) of the MySQL slow log file.
func (s *DatabaseService) SlowLogTail(connID, targetID string, lines int, sudoPassword string) (string, error) {
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return "", err
	}
	if t.Engine != EngineMySQL {
		return "", unsupported(t, "slowlog")
	}
	lines = max(10, min(lines, 5000))
	ctx, cancel := core.Timeout(statusTimeout)
	defer cancel()
	return s.mySlowLogTail(ctx, conn, t, lines, sudoPassword)
}

// CreateDatabase creates a database (owner: optional Postgres role).
func (s *DatabaseService) CreateDatabase(connID, targetID, name, owner, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return err
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err == nil {
		err = checkIdent(name)
	}
	if err == nil && owner != "" {
		err = checkIdent(owner)
	}
	if err == nil {
		ctx, cancel := core.Timeout(120 * time.Second)
		defer cancel()
		switch t.Engine {
		case EnginePostgres:
			q := "CREATE DATABASE " + pgQuoteIdent(name)
			if owner != "" {
				q += " OWNER " + pgQuoteIdent(owner)
			}
			_, err = s.runSQL(ctx, conn, t, "", []string{q}, sudoPassword)
		case EngineMySQL:
			_, err = s.runSQL(ctx, conn, t, "", []string{"CREATE DATABASE " + myQuoteIdent(name) + " CHARACTER SET utf8mb4"}, sudoPassword)
		default:
			err = unsupported(t, "createDatabase")
		}
	}
	detail := ""
	if owner != "" {
		detail = "owner " + owner
	}
	s.core.Audit(connID, "db.database.create", targetLabel(t, targetID)+"/"+name, detail, err)
	return err
}

// CreateUser creates a login role (Postgres) or user@host (MySQL, host
// defaults to localhost) with a password, optionally granting all
// privileges on a database. The password is sent on stdin only; Postgres
// receives a SCRAM verifier computed locally.
func (s *DatabaseService) CreateUser(connID, targetID, name, password, host, grantDatabase, sudoPassword string) error {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return err
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err == nil {
		err = checkIdent(name)
	}
	if err == nil && (password == "" || validSecret(password) != nil) {
		err = apperr.New("db.invalidPassword")
	}
	if err == nil && grantDatabase != "" {
		err = checkIdent(grantDatabase)
	}
	if host == "" {
		host = "localhost"
	}
	if err == nil && !reMyHost.MatchString(host) {
		err = apperr.New("db.invalidHost", "host", host)
	}
	if err == nil {
		ctx, cancel := core.Timeout(60 * time.Second)
		defer cancel()
		switch t.Engine {
		case EnginePostgres:
			var q string
			q, err = pgCreateUserSQL(name, password)
			if err == nil {
				stmts := []string{q}
				if grantDatabase != "" {
					stmts = append(stmts, "GRANT ALL PRIVILEGES ON DATABASE "+pgQuoteIdent(grantDatabase)+" TO "+pgQuoteIdent(name))
				}
				_, err = s.runSQL(ctx, conn, t, "", stmts, sudoPassword)
			}
		case EngineMySQL:
			if len(name) > 32 {
				err = apperr.New("db.invalidName", "name", name)
				break
			}
			acct := myQuoteLiteral(name) + "@" + myQuoteLiteral(host)
			stmts := []string{mySQLModePrelude, "CREATE USER " + acct + " IDENTIFIED BY " + myQuoteLiteral(password)}
			if grantDatabase != "" {
				stmts = append(stmts, "GRANT ALL PRIVILEGES ON "+myQuoteIdent(grantDatabase)+".* TO "+acct)
			}
			_, err = s.runSQL(ctx, conn, t, "", stmts, sudoPassword)
		default:
			err = unsupported(t, "createUser")
		}
	}
	detail := ""
	if t.Engine == EngineMySQL {
		detail = "host " + host
	}
	if grantDatabase != "" {
		detail = strings.TrimSpace(detail + " grant " + grantDatabase)
	}
	s.core.Audit(connID, "db.user.create", targetLabel(t, targetID)+"/"+name, detail, scrub(err, password))
	return scrubErr(err, password)
}

// scrub removes a secret from an error before it is logged or returned.
func scrub(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	ae := apperr.From(err)
	if strings.Contains(ae.Detail, secret) {
		c := *ae
		c.Detail = strings.ReplaceAll(c.Detail, secret, "***")
		return &c
	}
	return err
}

func scrubErr(err error, secret string) error { return scrub(err, secret) }

func targetLabel(t Target, id string) string {
	if t.Name != "" {
		return t.Name
	}
	return id
}

// RedisCommand runs one command in the Redis console. Commands that change
// data or server state need allowWrite (the UI asks for confirmation);
// KEYS on a database with more than 10000 keys needs it too.
func (s *DatabaseService) RedisCommand(connID, targetID string, db int, command string, allowWrite bool, sudoPassword string) (RedisResult, error) {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return RedisResult{}, err
	}
	conn, t, err := s.connTarget(connID, targetID)
	if err != nil {
		return RedisResult{}, err
	}
	if t.Engine != EngineRedis {
		return RedisResult{}, unsupported(t, "redis")
	}
	if db < 0 || db > 9999 {
		return RedisResult{}, apperr.New("db.invalidName", "name", strconv.Itoa(db))
	}
	if len(command) > 64<<10 {
		return RedisResult{}, apperr.New("db.queryTooLong")
	}
	args, err := splitRedisCommand(command)
	if err != nil {
		return RedisResult{}, err
	}
	if len(args) == 0 {
		return RedisResult{}, apperr.New("db.emptyQuery")
	}
	ctx, cancel := core.Timeout(60 * time.Second)
	defer cancel()
	r, err := s.redisCommand(ctx, conn, t, db, args, allowWrite, sudoPassword)
	outcome := err
	if err == nil && r.IsError {
		outcome = apperr.New("db.redis.failed").WithDetail(strings.TrimPrefix(r.Output, "(error) "))
	}
	// Refused commands (needs confirmation, blocked) never ran: not audited.
	if !apperr.HasCode(err, "db.redis.needConfirm") && !apperr.HasCode(err, "db.redis.keysLarge") && !apperr.HasCode(err, "db.redis.blocked") {
		action := "db.redis.read"
		if redisClass(args) == "write" {
			action = "db.redis.write"
		}
		s.core.Audit(connID, action, targetLabel(t, targetID)+fmt.Sprintf("/db%d", db), redactRedis(args), outcome)
	}
	return r, err
}

var (
	reSQLString = regexp.MustCompile(`'(?:[^'\\]|''|\\.)*'`)
	reSecretSQL = regexp.MustCompile(`(?i)password|identified|scram|md5`)
)

// redactSQL hides string literals of statements that carry passwords.
func redactSQL(sql string) string {
	if reSecretSQL.MatchString(sql) {
		sql = reSQLString.ReplaceAllString(sql, "'***'")
	}
	return shorten(sql, 2000)
}
