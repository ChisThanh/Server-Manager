package core

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"os/user"
	"strings"
	"time"

	"server-manager/internal/apperr"
)

// AuditEntry is one record of the tamper-evident audit log. Each entry's
// hash covers its content and the previous entry's hash, so editing or
// deleting a past record breaks the chain (see VerifyAudit).
type AuditEntry struct {
	ID         int64  `json:"id"`
	TS         int64  `json:"ts"` // unix ms
	Actor      string `json:"actor"`
	Server     string `json:"server"`
	ServerName string `json:"serverName"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	Detail     string `json:"detail"`
	OK         bool   `json:"ok"`
	Error      string `json:"error"`
	PrevHash   string `json:"prevHash"`
	Hash       string `json:"hash"`
}

// Actor names who is acting: the configured operator name, else the OS
// account ("tom@macbook").
func (c *Core) Actor() string {
	if n := strings.TrimSpace(c.Settings().OperatorName); n != "" {
		return n
	}
	return DefaultActor()
}

func DefaultActor() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if h, err := os.Hostname(); err == nil {
		name += "@" + strings.TrimSuffix(h, ".local")
	}
	return name
}

// Audit records an action. action is a dotted code ("service.restart"),
// target what it applied to ("nginx.service"), detail free text (commands,
// changed values — never secrets). err is the outcome (nil = success).
// Every audited action also appears on the server's event timeline.
func (c *Core) Audit(serverID, action, target, detail string, err error) {
	e := AuditEntry{
		TS:         time.Now().UnixMilli(),
		Actor:      c.Actor(),
		Server:     serverID,
		ServerName: c.ServerName(serverID),
		Action:     action,
		Target:     target,
		Detail:     truncate(detail, 4000),
		OK:         err == nil,
	}
	if serverID == "" {
		e.ServerName = ""
	}
	if err != nil {
		e.Error = truncate(apperr.From(err).Error(), 1000)
	}
	c.auditMu.Lock()
	var prev string
	_ = c.DB.QueryRow(`SELECT hash FROM audit ORDER BY id DESC LIMIT 1`).Scan(&prev)
	e.PrevHash = prev
	e.Hash = auditHash(e)
	_, dbErr := c.DB.Exec(`INSERT INTO audit(ts, actor, server, server_name, action, target, detail, ok, error, prev_hash, hash)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, e.TS, e.Actor, e.Server, e.ServerName, e.Action, e.Target, e.Detail, e.OK, e.Error, e.PrevHash, e.Hash)
	c.auditMu.Unlock()
	if dbErr != nil {
		logf("audit write failed: %v", dbErr)
	}
	if serverID != "" {
		sev := "info"
		if err != nil {
			sev = "warn"
		}
		c.AddEvent(Event{Server: serverID, Kind: "action", Severity: sev, Code: "action",
			Params: map[string]string{"action": action, "target": target}, Detail: e.Error, Actor: e.Actor})
	}
}

func auditHash(e AuditEntry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%t\x00%s",
		e.PrevHash, e.TS, e.Actor, e.Server, e.ServerName, e.Action, e.Target, e.Detail, e.OK, e.Error)
	return hex.EncodeToString(h.Sum(nil))
}

// AuditQuery filters the audit log.
type AuditQuery struct {
	Server string `json:"server"`
	Action string `json:"action"` // prefix match
	Text   string `json:"text"`   // matches actor/target/detail
	Since  int64  `json:"since"`  // unix ms
	Until  int64  `json:"until"`
	Limit  int    `json:"limit"`
	Before int64  `json:"before"` // id, for paging
}

func (c *Core) AuditList(q AuditQuery) ([]AuditEntry, error) {
	where := []string{"1=1"}
	args := []any{}
	if q.Server != "" {
		where = append(where, "server = ?")
		args = append(args, q.Server)
	}
	if q.Action != "" {
		where = append(where, "action LIKE ?")
		args = append(args, q.Action+"%")
	}
	if q.Text != "" {
		where = append(where, "(actor LIKE ? OR target LIKE ? OR detail LIKE ? OR server_name LIKE ?)")
		t := "%" + q.Text + "%"
		args = append(args, t, t, t, t)
	}
	if q.Since > 0 {
		where = append(where, "ts >= ?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		where = append(where, "ts <= ?")
		args = append(args, q.Until)
	}
	if q.Before > 0 {
		where = append(where, "id < ?")
		args = append(args, q.Before)
	}
	if q.Limit <= 0 || q.Limit > 5000 {
		q.Limit = 200
	}
	rows, err := c.DB.Query(`SELECT id, ts, actor, server, server_name, action, target, detail, ok, error, prev_hash, hash
		FROM audit WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Server, &e.ServerName, &e.Action, &e.Target, &e.Detail, &e.OK, &e.Error, &e.PrevHash, &e.Hash); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AuditVerification is the result of checking the hash chain.
type AuditVerification struct {
	OK       bool  `json:"ok"`
	Checked  int   `json:"checked"`
	BrokenAt int64 `json:"brokenAt"` // id of the first bad entry
}

// VerifyAudit recomputes the hash chain from the oldest retained entry.
func (c *Core) VerifyAudit() (AuditVerification, error) {
	rows, err := c.DB.Query(`SELECT id, ts, actor, server, server_name, action, target, detail, ok, error, prev_hash, hash FROM audit ORDER BY id`)
	if err != nil {
		return AuditVerification{}, err
	}
	defer rows.Close()
	v := AuditVerification{OK: true}
	prev := sql.NullString{}
	first := true
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Actor, &e.Server, &e.ServerName, &e.Action, &e.Target, &e.Detail, &e.OK, &e.Error, &e.PrevHash, &e.Hash); err != nil {
			return v, err
		}
		v.Checked++
		// The oldest retained entry may point at a pruned predecessor.
		if (!first && e.PrevHash != prev.String) || auditHash(e) != e.Hash {
			v.OK = false
			v.BrokenAt = e.ID
			return v, nil
		}
		first = false
		prev = sql.NullString{String: e.Hash, Valid: true}
	}
	return v, rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
