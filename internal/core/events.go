package core

import (
	"encoding/json"
	"log"
	"strings"
	"time"
)

// Event is an entry on a server's timeline (alerts, connection changes,
// service/container state changes, deployments, backups, user actions).
// Code + Params are translated by the frontend ("ev.<code>").
type Event struct {
	ID       int64             `json:"id"`
	TS       int64             `json:"ts"` // unix ms
	Server   string            `json:"server"`
	Kind     string            `json:"kind"`     // alert | conn | service | container | deploy | backup | action | security | health
	Severity string            `json:"severity"` // info | ok | warn | crit
	Code     string            `json:"code"`
	Params   map[string]string `json:"params"`
	Detail   string            `json:"detail"`
	Actor    string            `json:"actor"`
}

// AddEvent stores an event and pushes it to the frontend.
func (c *Core) AddEvent(e Event) {
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	if e.Severity == "" {
		e.Severity = "info"
	}
	if e.Params == nil {
		e.Params = map[string]string{}
	}
	p, _ := json.Marshal(e.Params)
	res, err := c.DB.Exec(`INSERT INTO events(ts, server, kind, severity, code, params, detail, actor) VALUES(?,?,?,?,?,?,?,?)`,
		e.TS, e.Server, e.Kind, e.Severity, e.Code, string(p), truncate(e.Detail, 2000), e.Actor)
	if err != nil {
		logf("event write failed: %v", err)
		return
	}
	e.ID, _ = res.LastInsertId()
	Emit(EventTimeline, e)
}

// EventQuery filters the timeline.
type EventQuery struct {
	Server   string   `json:"server"`
	Kinds    []string `json:"kinds"`
	Severity []string `json:"severity"`
	Since    int64    `json:"since"`
	Until    int64    `json:"until"`
	Limit    int      `json:"limit"`
	Before   int64    `json:"before"`
}

func (c *Core) Events(q EventQuery) ([]Event, error) {
	where := []string{"1=1"}
	args := []any{}
	if q.Server != "" {
		where = append(where, "server = ?")
		args = append(args, q.Server)
	}
	if len(q.Kinds) > 0 {
		where = append(where, "kind IN (?"+strings.Repeat(",?", len(q.Kinds)-1)+")")
		for _, k := range q.Kinds {
			args = append(args, k)
		}
	}
	if len(q.Severity) > 0 {
		where = append(where, "severity IN (?"+strings.Repeat(",?", len(q.Severity)-1)+")")
		for _, k := range q.Severity {
			args = append(args, k)
		}
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
	if q.Limit <= 0 || q.Limit > 2000 {
		q.Limit = 200
	}
	rows, err := c.DB.Query(`SELECT id, ts, server, kind, severity, code, params, detail, actor FROM events
		WHERE `+strings.Join(where, " AND ")+` ORDER BY ts DESC, id DESC LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var p string
		if err := rows.Scan(&e.ID, &e.TS, &e.Server, &e.Kind, &e.Severity, &e.Code, &p, &e.Detail, &e.Actor); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(p), &e.Params)
		if e.Params == nil {
			e.Params = map[string]string{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune deletes history older than the configured retention.
func (c *Core) Prune() {
	s := c.Settings()
	now := time.Now()
	if s.EventRetentionDays > 0 {
		_, _ = c.DB.Exec(`DELETE FROM events WHERE ts < ?`, now.AddDate(0, 0, -s.EventRetentionDays).UnixMilli())
	}
	if s.AuditRetentionDays > 0 {
		_, _ = c.DB.Exec(`DELETE FROM audit WHERE ts < ?`, now.AddDate(0, 0, -s.AuditRetentionDays).UnixMilli())
	}
	_, _ = c.DB.Exec(`DELETE FROM runs WHERE started < ?`, now.AddDate(0, 0, -365).UnixMilli())
}

func logf(format string, args ...any) {
	log.Printf("[core] "+format, args...)
}
