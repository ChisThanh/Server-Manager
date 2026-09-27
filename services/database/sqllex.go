package database

import (
	"strconv"
	"strings"

	"server-manager/internal/apperr"
)

// SQL lexing: splits user SQL into statements exactly as the command-line
// client would, so the runner can put a marker after each statement and
// tell result sets apart. It also enforces the client-safety rules:
//
//   - client meta-commands (psql `\! cmd`, `\o |cmd`, `\copy`, mysql
//     `\! cmd`, `source`…) start with a backslash outside a literal; they
//     would run with the client's (often root's) rights, so any backslash
//     outside a literal/comment is rejected;
//   - unterminated literals or comments are rejected (they would swallow
//     the runner's own statements);
//   - MySQL DELIMITER is not supported.
//
// The lexer is deliberately conservative: when in doubt it considers text
// to be outside a literal, which can only cause a rejection, never let a
// meta-command through.

type dialect int

const (
	dialectPG dialect = iota
	dialectMySQL
)

// stmt is one statement of user SQL.
type stmt struct {
	Text  string   // without the terminating ';'
	Words []string // upper-cased bare words outside literals/comments
}

func (s stmt) first() string {
	if len(s.Words) == 0 {
		return ""
	}
	return s.Words[0]
}

func (s stmt) has(word string) bool {
	for _, w := range s.Words {
		if w == word {
			return true
		}
	}
	return false
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

// splitSQL splits src into statements. Empty (comment-only) statements are
// dropped.
func splitSQL(src string, d dialect) ([]stmt, error) {
	var out []stmt
	start := 0
	depth := 0
	var words []string
	emit := func(end int) {
		text := strings.TrimSpace(src[start:end])
		if len(words) > 0 {
			out = append(out, stmt{Text: text, Words: words})
		}
		words = nil
	}
	n := len(src)
	lineStart := true
	for i := 0; i < n; {
		c := src[i]
		// MySQL client "DELIMITER" command at the start of a line.
		if d == dialectMySQL && lineStart {
			j := i
			for j < n && (src[j] == ' ' || src[j] == '\t') {
				j++
			}
			if j+9 <= n && strings.EqualFold(src[j:j+9], "delimiter") && (j+9 == n || src[j+9] == ' ' || src[j+9] == '\t') {
				return nil, apperr.New("db.delimiterUnsupported")
			}
		}
		lineStart = false
		switch {
		case c == '\n':
			lineStart = true
			i++
		case c == '\'' || (c == '"' && d == dialectMySQL):
			// Postgres: E'…' strings honour backslash escapes; plain ones
			// only '' (standard_conforming_strings is forced on).
			esc := d == dialectMySQL
			if d == dialectPG && i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i < 2 || !isIdentChar(src[i-2])) {
				esc = true
			}
			j, ok := skipQuoted(src, i, c, esc)
			if !ok {
				return nil, apperr.New("db.unterminated")
			}
			i = j
		case c == '"' || (c == '`' && d == dialectMySQL):
			j, ok := skipQuoted(src, i, c, false)
			if !ok {
				return nil, apperr.New("db.unterminated")
			}
			// Quoted identifiers count as words for the side-effect checks
			// ("pg_terminate_backend"(…) is still a call).
			if w := strings.ToUpper(src[i+1 : j-1]); w != "" {
				words = append(words, w)
			}
			i = j
		case c == '$' && d == dialectPG && (i == 0 || !isIdentChar(src[i-1])):
			if tag, ok := dollarTag(src, i); ok {
				end := strings.Index(src[i+len(tag):], tag)
				if end < 0 {
					return nil, apperr.New("db.unterminated")
				}
				i += len(tag) + end + len(tag)
			} else {
				i++
			}
		case c == '-' && i+1 < n && src[i+1] == '-' && (d == dialectPG || i+2 >= n || src[i+2] == ' ' || src[i+2] == '\t' || src[i+2] == '\n' || src[i+2] == '\r'):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i += j
			}
		case c == '#' && d == dialectMySQL:
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = n
			} else {
				i += j
			}
		case c == '/' && i+2 < n && src[i+1] == '*' && d == dialectMySQL && (src[i+2] == '!' || src[i+2] == '+' || (src[i+2] == 'M' && i+3 < n && src[i+3] == '!')):
			// Executable comment (/*!50100 … */, /*M!… */, hints): MySQL runs
			// its content, so lex it as code; the closing */ is harmless.
			i += 3
			for i < n && (src[i] == '!' || (src[i] >= '0' && src[i] <= '9')) {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			j, ok := skipBlockComment(src, i, d == dialectPG)
			if !ok {
				return nil, apperr.New("db.unterminated")
			}
			i = j
		case c == '\\':
			return nil, apperr.New("db.metaCommand")
		case c == '(':
			depth++
			i++
		case c == ')':
			if depth > 0 {
				depth--
			}
			i++
		case c == ';' && (depth == 0 || d == dialectMySQL):
			emit(i)
			i++
			start = i
			depth = 0
		case isIdentStart(c):
			j := i
			for j < n && isIdentChar(src[j]) {
				j++
			}
			// E'…' prefix belongs to the string that follows.
			if !(j-i == 1 && (c == 'E' || c == 'e') && j < n && src[j] == '\'') {
				words = append(words, strings.ToUpper(src[i:j]))
			}
			i = j
		default:
			i++
		}
	}
	emit(n)
	return out, nil
}

// skipQuoted returns the index after the literal starting at i (quote q).
// A doubled quote is an escaped quote; with esc, backslash escapes the next
// byte.
func skipQuoted(src string, i int, q byte, esc bool) (int, bool) {
	n := len(src)
	for j := i + 1; j < n; j++ {
		switch src[j] {
		case '\\':
			if esc {
				j++
			}
		case q:
			if j+1 < n && src[j+1] == q {
				j++
				continue
			}
			return j + 1, true
		}
	}
	return n, false
}

func skipBlockComment(src string, i int, nested bool) (int, bool) {
	n := len(src)
	lvl := 0
	for j := i; j < n; j++ {
		if src[j] == '/' && j+1 < n && src[j+1] == '*' {
			if lvl == 0 || nested {
				lvl++
			}
			j++
			continue
		}
		if src[j] == '*' && j+1 < n && src[j+1] == '/' {
			lvl--
			j++
			if lvl == 0 {
				return j + 1, true
			}
		}
	}
	return n, false
}

// dollarTag recognises a Postgres dollar-quote opener ($$ or $tag$) at i.
func dollarTag(src string, i int) (string, bool) {
	j := i + 1
	if j < len(src) && src[j] == '$' {
		return "$$", true
	}
	if j >= len(src) || !isIdentStart(src[j]) {
		return "", false
	}
	for j < len(src) && (isIdentStart(src[j]) || (src[j] >= '0' && src[j] <= '9')) {
		j++
	}
	if j < len(src) && src[j] == '$' {
		return src[i : j+1], true
	}
	return "", false
}

// Statements allowed in read-only mode (checked on top of the read-only
// transaction the server enforces).
var readOnlyFirst = map[dialect]map[string]bool{
	dialectPG: {"SELECT": true, "WITH": true, "SHOW": true, "EXPLAIN": true, "VALUES": true, "TABLE": true,
		"DECLARE": true, "FETCH": true, "MOVE": true, "CLOSE": true},
	dialectMySQL: {"SELECT": true, "WITH": true, "SHOW": true, "EXPLAIN": true, "DESCRIBE": true, "DESC": true,
		"VALUES": true, "TABLE": true, "HELP": true, "USE": true},
}

// Functions with side effects that a read-only transaction doesn't stop.
var pgSideEffects = map[string]bool{
	"PG_TERMINATE_BACKEND": true, "PG_CANCEL_BACKEND": true, "PG_RELOAD_CONF": true, "PG_ROTATE_LOGFILE": true,
	"LO_IMPORT": true, "LO_EXPORT": true, "LO_UNLINK": true, "PG_FILE_WRITE": true, "PG_FILE_UNLINK": true,
	"PG_FILE_RENAME": true, "DBLINK_EXEC": true, "DBLINK": true, "DBLINK_CONNECT": true, "PG_SWITCH_WAL": true,
	"PG_CREATE_RESTORE_POINT": true, "SET_CONFIG": true, "PG_PROMOTE": true, "PG_DROP_REPLICATION_SLOT": true,
	"PG_CREATE_PHYSICAL_REPLICATION_SLOT": true, "PG_CREATE_LOGICAL_REPLICATION_SLOT": true,
	"PG_STAT_RESET": true, "PG_STAT_RESET_SHARED": true, "PG_STAT_STATEMENTS_RESET": true,
	"PG_ADVISORY_LOCK": true, "PG_LOGICAL_EMIT_MESSAGE": true, "PG_REPLICATION_ORIGIN_CREATE": true,
	// these run SQL given as a string
	"QUERY_TO_XML": true, "QUERY_TO_XMLSCHEMA": true, "QUERY_TO_XML_AND_XMLSCHEMA": true, "CURSOR_TO_XML": true,
	"TABLE_TO_XML": true, "DATABASE_TO_XML": true, "SCHEMA_TO_XML": true, "TS_STAT": true,
}

// checkStatements validates split statements for the runner.
func checkStatements(stmts []stmt, d dialect, readOnly bool) error {
	for i, s := range stmts {
		n := i + 1
		first := s.first()
		if d == dialectPG && first == "COPY" && (s.has("STDIN") || s.has("STDOUT")) {
			return apperr.New("db.copyUnsupported", "n", itoa(n))
		}
		if d == dialectMySQL && s.has("LOCAL") && s.has("INFILE") {
			return apperr.New("db.localInfile", "n", itoa(n))
		}
		if !readOnly {
			continue
		}
		if !readOnlyFirst[d][first] {
			return apperr.New("db.readOnly", "n", itoa(n), "stmt", first)
		}
		if d == dialectMySQL && (s.has("OUTFILE") || s.has("DUMPFILE")) {
			return apperr.New("db.readOnly", "n", itoa(n), "stmt", "INTO OUTFILE")
		}
		if d == dialectMySQL && first == "SELECT" && s.has("FOR") && s.has("UPDATE") {
			return apperr.New("db.readOnly", "n", itoa(n), "stmt", "FOR UPDATE")
		}
		if d == dialectPG {
			for _, w := range s.Words {
				if pgSideEffects[w] {
					return apperr.New("db.readOnly", "n", itoa(n), "stmt", strings.ToLower(w))
				}
			}
		}
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
