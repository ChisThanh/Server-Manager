package database

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
	"server-manager/internal/sshx"
)

// StatementResult is the outcome of one statement of a query run.
type StatementResult struct {
	Index   int    `json:"index"`   // 1-based statement number
	SQL     string `json:"sql"`     // statement text (shortened)
	Command string `json:"command"` // first keyword (SELECT, UPDATE…)
	// HasRows: the statement returned a result set (possibly empty).
	HasRows bool        `json:"hasRows"`
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"` // nil cell = SQL NULL
	// RowCount: rows returned (result sets) or affected (DML); -1 unknown.
	RowCount  int64 `json:"rowCount"`
	Truncated bool  `json:"truncated"` // more rows than returned
}

// sqlReq is an internal request to run statements through the CLI client.
type sqlReq struct {
	database  string
	stmts     []string
	readOnly  bool
	timeoutMs int // statement timeout
	appName   string
	maxRows   int // rows kept per result set
	capBytes  int // output cap (0 = none)
}

// sqlOut is what came back.
type sqlOut struct {
	results   []StatementResult
	done      bool   // every statement ran
	errIndex  int    // statement that failed (1-based), 0 = none
	errMsg    string // the server's error message
	truncated bool   // output cap reached
}

var randRead = rand.Read

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	// don't cut a UTF-8 sequence
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// markerLine is one marker printed by a runner.
type markerLine struct {
	kind  string // B (connected), R (statement done), E (end)
	arg   string
	start int // offset of the marker line
	end   int // offset after its newline
}

// findMarkers locates the marker lines "<mark> <kind> [arg]" in out.
func findMarkers(out, mark string) []markerLine {
	var ms []markerLine
	pos := 0
	for {
		i := strings.Index(out[pos:], mark)
		if i < 0 {
			return ms
		}
		i += pos
		if i > 0 && out[i-1] != '\n' {
			pos = i + len(mark)
			continue
		}
		e := strings.IndexByte(out[i:], '\n')
		lineEnd := len(out)
		if e >= 0 {
			lineEnd = i + e
		}
		f := strings.Fields(out[i+len(mark) : lineEnd])
		m := markerLine{start: i, end: min(lineEnd+1, len(out))}
		if len(f) > 0 {
			m.kind = f[0]
		}
		if len(f) > 1 {
			m.arg = f[1]
		}
		ms = append(ms, m)
		pos = m.end
	}
}

// ---- PostgreSQL ----

func (s *DatabaseService) pgArgs(t Target, database string) []string {
	args := []string{"-X", "-q", "-w", "-v", "ON_ERROR_STOP=1"}
	if t.Host != "" {
		args = append(args, "-h", t.Host)
	}
	if t.Port > 0 {
		args = append(args, "-p", strconv.Itoa(t.Port))
	}
	if t.Auth == AuthPassword && t.User != "" {
		args = append(args, "-U", t.User)
	}
	if database == "" {
		database = t.Database
	}
	if database == "" {
		database = "postgres"
	}
	return append(args, "-d", database)
}

func pgEnv(appName string, readOnly bool, timeoutMs int) []envVar {
	opts := "-c standard_conforming_strings=on"
	if timeoutMs > 0 {
		opts += " -c statement_timeout=" + strconv.Itoa(timeoutMs)
	}
	if readOnly {
		opts += " -c default_transaction_read_only=on"
	}
	return []envVar{{"PGAPPNAME", appName}, {"PGCONNECT_TIMEOUT", "10"}, {"PGCLIENTENCODING", "UTF8"},
		{"LC_MESSAGES", "C"}, {"PGOPTIONS", opts}}
}

func (s *DatabaseService) runPG(ctx context.Context, conn *sshx.Conn, t Target, r sqlReq, sudoPW string) (sqlOut, error) {
	out, err := s.runPGFormat(ctx, conn, t, r, sudoPW, true)
	if err == nil && !out.done && strings.Contains(out.errMsg, "allowed formats") {
		// psql < 12 has no CSV output.
		return s.runPGFormat(ctx, conn, t, r, sudoPW, false)
	}
	return out, err
}

func (s *DatabaseService) runPGFormat(ctx context.Context, conn *sshx.Conn, t Target, r sqlReq, sudoPW string, csv bool) (sqlOut, error) {
	nonce := randomHex(8)
	mark := markerPrefix + nonce
	nullTok := markerPrefix + "NULL" + nonce
	var b strings.Builder
	if csv {
		b.WriteString("\\pset format csv\n")
	} else {
		b.WriteString("\\pset format unaligned\n\\pset fieldsep '\\037'\n\\pset recordsep '\\036'\n")
	}
	b.WriteString("\\pset null '" + nullTok + "'\n\\pset footer off\n\\pset pager off\n\\set FETCH_COUNT 1000\n")
	if r.readOnly {
		b.WriteString("BEGIN TRANSACTION READ ONLY;\n")
	}
	b.WriteString("\\echo " + mark + " B\n")
	for _, st := range r.stmts {
		b.WriteString(st + "\n;\n\\echo " + mark + " R :ROW_COUNT\n")
	}
	if r.readOnly {
		b.WriteString("COMMIT;\n")
	}
	b.WriteString("\\echo " + mark + " E\n")
	res, err := s.runClient(ctx, conn, t, s.pgArgs(t, r.database), pgEnv(r.appName, r.readOnly, r.timeoutMs), b.String(), r.capBytes, sudoPW)
	if err != nil {
		return sqlOut{}, err
	}
	o := collect(res, mark, r, func(seg string) (bool, []string, [][]*string, int) {
		if csv {
			return parsePGCSV(seg, nullTok, r.maxRows)
		}
		return parsePGUnaligned(seg, nullTok, r.maxRows)
	})
	return o, nil
}

// collect splits runner output at the markers and parses each statement's
// segment with parse (hasRows, columns, rows kept, total rows).
func collect(res sshx.ExecResult, mark string, r sqlReq, parse func(seg string) (bool, []string, [][]*string, int)) sqlOut {
	o := sqlOut{results: []StatementResult{}}
	ms := findMarkers(res.Stdout, mark)
	begun := false
	segStart := 0
	idx := 0
	for _, m := range ms {
		switch m.kind {
		case "B":
			begun = true
			segStart = m.end
		case "R":
			if !begun || idx >= len(r.stmts) {
				continue
			}
			seg := res.Stdout[segStart:m.start]
			segStart = m.end
			sr := StatementResult{Index: idx + 1, SQL: shorten(r.stmts[idx], 300), Command: firstWord(r.stmts[idx]), Columns: []string{}, Rows: [][]*string{}, RowCount: -1}
			if n, err := strconv.ParseInt(m.arg, 10, 64); err == nil {
				sr.RowCount = n
			}
			has, cols, rows, total := parse(seg)
			if has {
				sr.HasRows = true
				sr.Columns = cols
				sr.Rows = rows
				sr.RowCount = int64(total)
				sr.Truncated = total > len(rows)
			}
			o.results = append(o.results, sr)
			idx++
		case "E":
			if begun && idx == len(r.stmts) {
				o.done = true
			}
		}
	}
	if !o.done {
		o.errIndex = idx + 1
		if idx >= len(r.stmts) {
			o.errIndex = len(r.stmts) // failed at COMMIT
		}
		o.errMsg = statementError(res.Stderr)
		if o.errMsg == "" {
			if r.capBytes > 0 && len(res.Stdout) >= r.capBytes-1 {
				o.truncated = true
			}
			o.errMsg = strings.TrimSpace(firstLines(res.Stdout, 3))
		}
	}
	return o
}

var reMyErrLine = regexp.MustCompile(`ERROR (\d+(?: \([0-9A-Z]+\))?) at line \d+:`)

// statementError extracts the error message from client stderr, dropping
// MySQL's echo of the failing statement and harmless warnings.
func statementError(stderr string) string {
	s := strings.TrimSpace(stderr)
	// "ERROR 1146 (42S02) at line 7: …": the line is the runner script's
	s = reMyErrLine.ReplaceAllString(s, "ERROR $1:")
	if i := strings.LastIndex(s, "--------------\n"); i >= 0 {
		s = strings.TrimSpace(s[i+len("--------------\n"):])
	}
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "Using a password on the command line") || strings.Contains(l, "MYSQL_PWD") && strings.Contains(l, "deprecated") ||
			strings.Contains(l, "Deprecated program name") {
			continue
		}
		keep = append(keep, l)
	}
	return shorten(strings.Join(keep, "\n"), 4000)
}

func firstWord(sql string) string {
	st, err := splitSQL(sql, dialectPG)
	if err == nil && len(st) > 0 {
		return st[0].first()
	}
	f := strings.Fields(sql)
	if len(f) == 0 {
		return ""
	}
	return strings.ToUpper(f[0])
}

// parseCSV parses psql's CSV output: fields separated by ',', records by
// '\n'; fields holding ',', '"', '\r' or '\n' are double-quoted with '""'
// escapes. Unquoted fields equal to null are NULL.
func parseCSV(text, null string, visit func(rec []*string) bool) {
	i, n := 0, len(text)
	var rec []*string
	for i < n {
		var val string
		quoted := false
		if text[i] == '"' {
			quoted = true
			var sb strings.Builder
			i++
			for i < n {
				c := text[i]
				if c == '"' {
					if i+1 < n && text[i+1] == '"' {
						sb.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				sb.WriteByte(c)
				i++
			}
			val = sb.String()
		} else {
			j := i
			for j < n && text[j] != ',' && text[j] != '\n' {
				j++
			}
			val = text[i:j]
			i = j
		}
		if !quoted && val == null {
			rec = append(rec, nil)
		} else {
			v := val
			rec = append(rec, &v)
		}
		if i >= n || text[i] == '\n' {
			if !visit(rec) {
				return
			}
			rec = nil
			i++
			continue
		}
		i++ // ','
		if i >= n {
			// trailing separator: one more empty field
			e := ""
			rec = append(rec, &e)
			visit(rec)
			return
		}
	}
}

func parsePGCSV(seg, null string, maxRows int) (bool, []string, [][]*string, int) {
	if seg == "" {
		return false, nil, nil, 0
	}
	var cols []string
	rows := [][]*string{}
	total := 0
	first := true
	parseCSV(seg, null, func(rec []*string) bool {
		if first {
			first = false
			for _, c := range rec {
				if c == nil {
					cols = append(cols, "")
				} else {
					cols = append(cols, *c)
				}
			}
			return true
		}
		total++
		if len(rows) < maxRows {
			rows = append(rows, rec)
		}
		return true
	})
	if cols == nil {
		cols = []string{}
	}
	return true, cols, rows, total
}

// parsePGUnaligned parses psql's unaligned output with US/RS separators
// (psql < 12). Values containing those control characters can't be told
// apart; such clients are long unsupported.
func parsePGUnaligned(seg, null string, maxRows int) (bool, []string, [][]*string, int) {
	seg = strings.TrimSuffix(seg, "\n")
	if seg == "" {
		return false, nil, nil, 0
	}
	recs := strings.Split(seg, "\x1e")
	cols := strings.Split(recs[0], "\x1f")
	rows := [][]*string{}
	for _, r := range recs[1:] {
		if len(rows) >= maxRows {
			break
		}
		var row []*string
		for _, f := range strings.Split(r, "\x1f") {
			if f == null {
				row = append(row, nil)
			} else {
				v := f
				row = append(row, &v)
			}
		}
		rows = append(rows, row)
	}
	return true, cols, rows, len(recs) - 1
}

// ---- MySQL / MariaDB ----

func (s *DatabaseService) myArgs(t Target, database string) []string {
	args := []string{"--xml", "--batch", "--binary-mode", "--quick", "--local-infile=0", "--connect-timeout=10",
		"--default-character-set=utf8mb4", "--no-auto-rehash"}
	host := t.Host
	if host == "" && t.Port > 0 {
		host = "127.0.0.1"
	}
	if strings.HasPrefix(host, "/") {
		args = append(args, "--socket="+host)
	} else if host != "" {
		args = append(args, "--host="+host)
	}
	if t.Port > 0 {
		args = append(args, "--port="+strconv.Itoa(t.Port))
	}
	if t.Auth == AuthPassword && t.User != "" {
		args = append(args, "--user="+t.User)
	}
	if database == "" {
		database = t.Database
	}
	if database != "" {
		args = append(args, "--database="+database)
	}
	return args
}

// mysqlFlavor returns "mariadb" or "mysql" for the target's server (cached).
func (s *DatabaseService) mysqlFlavor(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) (string, error) {
	key := conn.ID + "/" + t.ID
	s.mu.Lock()
	f, ok := s.flavors[key]
	s.mu.Unlock()
	if ok {
		return f, nil
	}
	o, err := s.runMySQLRaw(ctx, conn, t, sqlReq{stmts: []string{"SELECT VERSION() AS v"}, maxRows: 1}, "", sudoPW)
	if err != nil {
		return "", err
	}
	if !o.done || len(o.results) == 0 || len(o.results[0].Rows) == 0 || o.results[0].Rows[0][0] == nil {
		return "", apperr.New("db.connectFailed").WithDetail(o.errMsg)
	}
	f = "mysql"
	if strings.Contains(strings.ToLower(*o.results[0].Rows[0][0]), "mariadb") {
		f = "mariadb"
	}
	s.mu.Lock()
	s.flavors[key] = f
	s.mu.Unlock()
	return f, nil
}

func (s *DatabaseService) runMySQL(ctx context.Context, conn *sshx.Conn, t Target, r sqlReq, sudoPW string) (sqlOut, error) {
	flavor := ""
	if r.timeoutMs > 0 {
		f, err := s.mysqlFlavor(ctx, conn, t, sudoPW)
		if err != nil {
			return sqlOut{}, err
		}
		flavor = f
	}
	return s.runMySQLRaw(ctx, conn, t, r, flavor, sudoPW)
}

func (s *DatabaseService) runMySQLRaw(ctx context.Context, conn *sshx.Conn, t Target, r sqlReq, flavor, sudoPW string) (sqlOut, error) {
	nonce := randomHex(8)
	mark := markerPrefix + nonce
	marker := func(kind string, rows bool) string {
		if rows {
			return "SELECT '" + mark + " " + kind + "' AS sm_marker, ROW_COUNT() AS sm_rows;\n"
		}
		return "SELECT '" + mark + " " + kind + "' AS sm_marker;\n"
	}
	var b strings.Builder
	if r.readOnly {
		b.WriteString("SET SESSION TRANSACTION READ ONLY;\n")
	}
	if r.timeoutMs > 0 {
		switch flavor {
		case "mariadb":
			b.WriteString("SET SESSION max_statement_time=" + strconv.FormatFloat(float64(r.timeoutMs)/1000, 'f', 3, 64) + ";\n")
		case "mysql":
			b.WriteString("SET SESSION max_execution_time=" + strconv.Itoa(r.timeoutMs) + ";\n")
		}
	}
	if r.readOnly {
		b.WriteString("START TRANSACTION READ ONLY;\n")
	}
	b.WriteString(marker("B", false))
	for _, st := range r.stmts {
		b.WriteString(st + "\n;\n" + marker("R", true))
	}
	if r.readOnly {
		b.WriteString("COMMIT;\n")
	}
	b.WriteString(marker("E", false))
	res, err := s.runClient(ctx, conn, t, s.myArgs(t, r.database), nil, b.String(), r.capBytes, sudoPW)
	if err != nil {
		return sqlOut{}, err
	}
	return collectMySQL(res, mark, r), nil
}

// collectMySQL walks the XML result sets: marker sets delimit statements.
func collectMySQL(res sshx.ExecResult, mark string, r sqlReq) sqlOut {
	o := sqlOut{results: []StatementResult{}}
	sets := parseMyXML(res.Stdout, r.maxRows)
	begun := false
	idx := 0
	var pending []xmlSet
	for _, set := range sets {
		if len(set.cols) > 0 && set.cols[0] == "sm_marker" && len(set.rows) == 1 && set.rows[0][0] != nil && strings.HasPrefix(*set.rows[0][0], mark+" ") {
			kind := strings.TrimPrefix(*set.rows[0][0], mark+" ")
			switch kind {
			case "B":
				begun = true
				pending = nil
			case "R":
				if !begun || idx >= len(r.stmts) {
					continue
				}
				var affected int64 = -1
				if len(set.rows[0]) > 1 && set.rows[0][1] != nil {
					affected, _ = strconv.ParseInt(*set.rows[0][1], 10, 64)
				}
				base := StatementResult{Index: idx + 1, SQL: shorten(r.stmts[idx], 300), Command: myFirstWord(r.stmts[idx]), Columns: []string{}, Rows: [][]*string{}, RowCount: affected}
				if len(pending) == 0 {
					o.results = append(o.results, base)
				}
				for _, p := range pending {
					sr := base
					sr.HasRows = true
					sr.Columns = p.cols
					if sr.Columns == nil {
						sr.Columns = []string{}
					}
					sr.Rows = p.rows
					sr.RowCount = int64(p.total)
					sr.Truncated = p.total > len(p.rows)
					o.results = append(o.results, sr)
				}
				pending = nil
				idx++
			case "E":
				if begun && idx == len(r.stmts) {
					o.done = true
				}
			}
			continue
		}
		if begun {
			pending = append(pending, set)
		}
	}
	if !o.done {
		o.errIndex = idx + 1
		if idx >= len(r.stmts) {
			o.errIndex = len(r.stmts)
		}
		o.errMsg = statementError(res.Stderr)
		if o.errMsg == "" {
			if r.capBytes > 0 && len(res.Stdout) >= r.capBytes-1 {
				o.truncated = true
			}
		}
	}
	return o
}

func myFirstWord(sql string) string {
	st, err := splitSQL(sql, dialectMySQL)
	if err == nil && len(st) > 0 {
		return st[0].first()
	}
	return firstWord(sql)
}

type xmlSet struct {
	cols  []string
	rows  [][]*string
	total int
}

// parseMyXML parses `mysql --xml` output. The client escapes <, >, & and
// " in names and values, so tags can be found by plain search; NULL is
// <field name="x" xsi:nil="true" />. Rows past maxRows are only counted.
func parseMyXML(out string, maxRows int) []xmlSet {
	var sets []xmlSet
	pos := 0
	for {
		i := strings.Index(out[pos:], "<resultset ")
		if i < 0 {
			return sets
		}
		i += pos
		gt := strings.IndexByte(out[i:], '>')
		if gt < 0 {
			return sets
		}
		bodyStart := i + gt + 1
		end := strings.Index(out[bodyStart:], "</resultset>")
		body := ""
		if end < 0 {
			body = out[bodyStart:]
			pos = len(out)
		} else {
			body = out[bodyStart : bodyStart+end]
			pos = bodyStart + end + len("</resultset>")
		}
		if strings.HasSuffix(out[i:bodyStart], "/>") {
			body = ""
		}
		set := xmlSet{rows: [][]*string{}}
		rp := 0
		for {
			r := strings.Index(body[rp:], "<row>")
			if r < 0 {
				break
			}
			r += rp
			re := strings.Index(body[r:], "</row>")
			if re < 0 {
				re = len(body) - r
			}
			rowText := body[r+5 : r+re]
			rp = r + re
			set.total++
			if len(set.rows) >= maxRows && set.cols != nil {
				continue
			}
			cols, vals := parseMyRow(rowText)
			if set.cols == nil {
				set.cols = cols
			}
			if len(set.rows) < maxRows {
				set.rows = append(set.rows, vals)
			}
		}
		sets = append(sets, set)
		if end < 0 {
			return sets
		}
	}
}

func parseMyRow(text string) ([]string, []*string) {
	var cols []string
	var vals []*string
	p := 0
	const open = `<field name="`
	for {
		i := strings.Index(text[p:], open)
		if i < 0 {
			return cols, vals
		}
		i += p + len(open)
		q := strings.IndexByte(text[i:], '"')
		if q < 0 {
			return cols, vals
		}
		cols = append(cols, xmlUnescape(text[i:i+q]))
		rest := text[i+q+1:]
		if strings.HasPrefix(rest, " xsi:nil=\"true\"") {
			vals = append(vals, nil)
			e := strings.Index(rest, "/>")
			if e < 0 {
				return cols, vals
			}
			p = i + q + 1 + e + 2
			continue
		}
		if strings.HasPrefix(rest, " />") || strings.HasPrefix(rest, "/>") {
			v := ""
			vals = append(vals, &v)
			p = i + q + 1 + strings.Index(rest, "/>") + 2
			continue
		}
		if !strings.HasPrefix(rest, ">") {
			return cols, vals
		}
		ve := strings.Index(rest, "</field>")
		if ve < 0 {
			return cols, vals
		}
		v := xmlUnescape(rest[1:ve])
		vals = append(vals, &v)
		p = i + q + 1 + ve + len("</field>")
	}
}

func xmlUnescape(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}
		e := strings.IndexByte(s[i:], ';')
		if e < 0 || e > 10 {
			b.WriteByte('&')
			continue
		}
		ent := s[i+1 : i+e]
		switch {
		case ent == "lt":
			b.WriteByte('<')
		case ent == "gt":
			b.WriteByte('>')
		case ent == "amp":
			b.WriteByte('&')
		case ent == "quot":
			b.WriteByte('"')
		case ent == "apos":
			b.WriteByte('\'')
		case strings.HasPrefix(ent, "#x") || strings.HasPrefix(ent, "#X"):
			if v, err := strconv.ParseUint(ent[2:], 16, 32); err == nil {
				b.WriteRune(rune(v))
			} else {
				b.WriteString("&" + ent + ";")
			}
		case strings.HasPrefix(ent, "#"):
			if v, err := strconv.ParseUint(ent[1:], 10, 32); err == nil {
				b.WriteRune(rune(v))
			} else {
				b.WriteString("&" + ent + ";")
			}
		default:
			b.WriteString("&" + ent + ";")
		}
		i += e
	}
	return b.String()
}

// ---- helpers for internal queries ----

// runSQL runs internal (trusted) statements on a Postgres or MySQL target
// and returns their results; a statement error becomes db.queryFailed.
func (s *DatabaseService) runSQL(ctx context.Context, conn *sshx.Conn, t Target, database string, stmts []string, sudoPW string) ([]StatementResult, error) {
	r := sqlReq{database: database, stmts: stmts, readOnly: false, timeoutMs: 15000, appName: "server-manager", maxRows: 5000}
	var o sqlOut
	var err error
	switch t.Engine {
	case EnginePostgres:
		o, err = s.runPG(ctx, conn, t, r, sudoPW)
	case EngineMySQL:
		o, err = s.runMySQL(ctx, conn, t, r, sudoPW)
	default:
		return nil, apperr.New("db.unsupported", "engine", t.Engine)
	}
	if err != nil {
		return nil, err
	}
	if !o.done {
		return o.results, apperr.New("db.queryFailed", "n", strconv.Itoa(o.errIndex)).WithDetail(o.errMsg)
	}
	return o.results, nil
}

// cell returns row[i] as a string ("" for NULL or out of range).
func cell(row []*string, i int) string {
	if i < len(row) && row[i] != nil {
		return *row[i]
	}
	return ""
}

func cellInt(row []*string, i int) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(cell(row, i)), 10, 64)
	return v
}

func cellFloat(row []*string, i int) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(cell(row, i)), 64)
	return v
}

func cellBool(row []*string, i int) bool {
	v := strings.ToLower(cell(row, i))
	return v == "t" || v == "true" || v == "1" || v == "on" || v == "yes"
}

// rowsOf returns the rows of the i-th result (or none).
func rowsOf(rs []StatementResult, i int) [][]*string {
	if i < len(rs) {
		return rs[i].Rows
	}
	return nil
}
