package database

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
)

// QueryRequest is a query-runner request (Postgres, MySQL).
type QueryRequest struct {
	TargetID string `json:"targetId"`
	Database string `json:"database"` // "" = target default
	SQL      string `json:"sql"`
	// AllowWrite runs without the read-only guard (the UI confirms first).
	AllowWrite bool `json:"allowWrite"`
	TimeoutSec int  `json:"timeoutSec"` // statement timeout, default 30, max 3600
	MaxRows    int  `json:"maxRows"`    // rows kept per result, default 1000, max 10000
}

// QueryResult is the outcome of a run. Statements before a failing one
// keep their results; ErrorIndex/Error describe the failure.
type QueryResult struct {
	Statements []StatementResult `json:"statements"`
	Elapsed    int64             `json:"elapsed"` // ms
	ReadOnly   bool              `json:"readOnly"`
	ErrorIndex int               `json:"errorIndex"` // 1-based, 0 = none
	Error      string            `json:"error"`
	// OutputTruncated: the output was too large and got cut.
	OutputTruncated bool `json:"outputTruncated"`
}

// HistoryEntry is a past query run of a target.
type HistoryEntry struct {
	ID       string `json:"id"`
	TS       int64  `json:"ts"` // unix ms
	SQL      string `json:"sql"`
	Database string `json:"database"`
	OK       bool   `json:"ok"`
	Error    string `json:"error"`
	Elapsed  int64  `json:"elapsed"`
	Rows     int64  `json:"rows"`
	ReadOnly bool   `json:"readOnly"`
}

// Snippet is a saved query.
type Snippet struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Engine  string `json:"engine"` // postgres | mysql | redis
	SQL     string `json:"sql"`
	Updated int64  `json:"updated"`
}

const (
	maxQueryLen   = 1 << 20
	historyLimit  = 50
	queryCapBytes = 6 << 20
)

// RunQuery runs SQL on a Postgres or MySQL target. Read-only by default:
// only reading statements are accepted and they run in a read-only
// transaction with default_transaction_read_only / SET SESSION TRANSACTION
// READ ONLY. Every run is audited and added to the target's history.
func (s *DatabaseService) RunQuery(connID string, req QueryRequest, sudoPassword string) (QueryResult, error) {
	if err := s.core.Require(connID, core.PermDatabase); err != nil {
		return QueryResult{}, err
	}
	t, err := s.target(connID, req.TargetID)
	if err != nil {
		return QueryResult{}, err
	}
	res, err := s.runQuery(connID, t, req, sudoPassword)
	outcome := err
	if err == nil && res.ErrorIndex > 0 {
		outcome = apperr.New("db.queryFailed", "n", strconv.Itoa(res.ErrorIndex)).WithDetail(shorten(res.Error, 500))
	}
	mode := "read-only"
	if req.AllowWrite {
		mode = "write"
	}
	dbName := core.FirstNonEmpty(req.Database, t.Database)
	s.core.Audit(connID, "db.query.run", targetLabel(t, req.TargetID)+"/"+dbName, mode+": "+redactSQL(req.SQL), outcome)
	s.addHistory(connID, req.TargetID, req, res, outcome)
	return res, err
}

func (s *DatabaseService) runQuery(connID string, t Target, req QueryRequest, sudoPW string) (QueryResult, error) {
	var d dialect
	switch t.Engine {
	case EnginePostgres:
		d = dialectPG
	case EngineMySQL:
		d = dialectMySQL
	default:
		return QueryResult{}, unsupported(t, "query")
	}
	if strings.TrimSpace(req.SQL) == "" {
		return QueryResult{}, apperr.New("db.emptyQuery")
	}
	if len(req.SQL) > maxQueryLen {
		return QueryResult{}, apperr.New("db.queryTooLong")
	}
	if req.Database != "" {
		if err := checkDBName(req.Database); err != nil {
			return QueryResult{}, err
		}
	}
	stmts, err := splitSQL(req.SQL, d)
	if err != nil {
		return QueryResult{}, err
	}
	if len(stmts) == 0 {
		return QueryResult{}, apperr.New("db.emptyQuery")
	}
	if err := checkStatements(stmts, d, !req.AllowWrite); err != nil {
		return QueryResult{}, err
	}
	timeout := req.TimeoutSec
	if timeout <= 0 {
		timeout = 30
	}
	timeout = min(timeout, 3600)
	maxRows := req.MaxRows
	if maxRows <= 0 {
		maxRows = 1000
	}
	maxRows = min(maxRows, 10000)
	texts := make([]string, len(stmts))
	for i, st := range stmts {
		texts[i] = st.Text
	}
	r := sqlReq{database: req.Database, stmts: texts, readOnly: !req.AllowWrite, timeoutMs: timeout * 1000,
		appName: "server-manager-query", maxRows: maxRows, capBytes: queryCapBytes}
	ctx, cancel := core.Timeout(time.Duration(timeout)*time.Second + 45*time.Second)
	defer cancel()
	c, err := s.core.Conn(connID)
	if err != nil {
		return QueryResult{}, err
	}
	start := time.Now()
	var o sqlOut
	if d == dialectPG {
		o, err = s.runPG(ctx, c, t, r, sudoPW)
	} else {
		o, err = s.runMySQL(ctx, c, t, r, sudoPW)
	}
	if err != nil {
		return QueryResult{}, err
	}
	res := QueryResult{Statements: o.results, Elapsed: time.Since(start).Milliseconds(), ReadOnly: r.readOnly, OutputTruncated: o.truncated}
	if !o.done {
		res.ErrorIndex = max(o.errIndex, 1)
		res.Error = o.errMsg
		if res.Error == "" && o.truncated {
			res.Error = "output too large"
		}
		if res.Error == "" {
			res.Error = "unknown error"
		}
	}
	return res, nil
}

func (s *DatabaseService) addHistory(connID, targetID string, req QueryRequest, res QueryResult, outcome error) {
	var rows int64
	for _, st := range res.Statements {
		if st.RowCount > 0 {
			rows += st.RowCount
		}
	}
	e := HistoryEntry{ID: uuid.NewString(), TS: time.Now().UnixMilli(), SQL: shorten(redactHistory(req.SQL), 20000), Database: req.Database,
		OK: outcome == nil, Elapsed: res.Elapsed, Rows: rows, ReadOnly: !req.AllowWrite}
	if outcome != nil {
		e.Error = shorten(apperr.From(outcome).Error(), 500)
	}
	key := connID + "/" + targetID
	s.mu.Lock()
	defer s.mu.Unlock()
	var list []HistoryEntry
	_ = s.core.DB.Get(nsHistory, key, &list)
	// consecutive duplicates collapse into the newest run
	if len(list) > 0 && list[0].SQL == e.SQL && list[0].Database == e.Database {
		list = list[1:]
	}
	list = append([]HistoryEntry{e}, list...)
	if len(list) > historyLimit {
		list = list[:historyLimit]
	}
	_ = s.core.DB.Put(nsHistory, key, list)
}

// redactHistory keeps history free of passwords.
func redactHistory(sql string) string {
	if reSecretSQL.MatchString(sql) {
		return reSQLString.ReplaceAllString(sql, "'***'")
	}
	return sql
}

// History returns the last query runs of a target (newest first).
func (s *DatabaseService) History(connID, targetID string) ([]HistoryEntry, error) {
	var list []HistoryEntry
	err := s.core.DB.Get(nsHistory, connID+"/"+targetID, &list)
	if err != nil && err != db.ErrNotFound {
		return nil, err
	}
	if list == nil {
		list = []HistoryEntry{}
	}
	return list, nil
}

// ClearHistory forgets the query history of a target.
func (s *DatabaseService) ClearHistory(connID, targetID string) error {
	return s.core.DB.Delete(nsHistory, connID+"/"+targetID)
}

// Snippets lists saved queries for an engine ("" = all).
func (s *DatabaseService) Snippets(engine string) ([]Snippet, error) {
	list, err := db.List[Snippet](s.core.DB, nsSnippet)
	if err != nil {
		return nil, err
	}
	out := []Snippet{}
	for _, sn := range list {
		if engine == "" || sn.Engine == engine {
			out = append(out, sn)
		}
	}
	return out, nil
}

// SaveSnippet creates or updates a saved query.
func (s *DatabaseService) SaveSnippet(sn Snippet) (Snippet, error) {
	sn.Name = strings.TrimSpace(sn.Name)
	if sn.Name == "" || len(sn.Name) > 120 {
		return Snippet{}, apperr.New("db.invalidName", "name", sn.Name)
	}
	if !validEngine(sn.Engine) {
		return Snippet{}, apperr.New("db.invalidEngine", "engine", sn.Engine)
	}
	if len(sn.SQL) > maxQueryLen {
		return Snippet{}, apperr.New("db.queryTooLong")
	}
	if sn.ID == "" {
		sn.ID = uuid.NewString()
	} else if !reTargetID.MatchString(sn.ID) {
		return Snippet{}, apperr.New("db.invalidName", "name", sn.ID)
	}
	sn.Updated = time.Now().Unix()
	if err := s.core.DB.Put(nsSnippet, sn.ID, sn); err != nil {
		return Snippet{}, err
	}
	return sn, nil
}

// DeleteSnippet removes a saved query.
func (s *DatabaseService) DeleteSnippet(id string) error {
	return s.core.DB.Delete(nsSnippet, id)
}
