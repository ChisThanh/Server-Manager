package services

import (
	"encoding/csv"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/core"
)

// AppService exposes app-wide features: settings, audit log, event
// timeline and background jobs.
type AppService struct {
	core *Core
}

func NewAppService(c *Core) *AppService { return &AppService{core: c} }

func (s *AppService) GetSettings() core.Settings { return s.core.Settings() }

func (s *AppService) SaveSettings(in core.Settings) (core.Settings, error) {
	old := s.core.Settings()
	out, err := s.core.SaveSettings(in)
	if err == nil && old.OperatorName != out.OperatorName {
		s.core.Audit("", "settings.operator", out.OperatorName, "was: "+old.OperatorName, nil)
	}
	return out, err
}

// DefaultActor is the identity used when no operator name is set.
func (s *AppService) DefaultActor() string { return core.DefaultActor() }

// RolePerms lists what each server role may do.
func (s *AppService) RolePerms() map[string][]string { return core.RolePerms() }

// Allowed reports whether the server's role grants perm.
func (s *AppService) Allowed(serverID, perm string) bool {
	return s.core.Allowed(serverID, core.Perm(perm))
}

func (s *AppService) Audit(q core.AuditQuery) ([]core.AuditEntry, error) { return s.core.AuditList(q) }

func (s *AppService) VerifyAudit() (core.AuditVerification, error) { return s.core.VerifyAudit() }

// ExportAudit writes the (filtered) audit log to a CSV file chosen by the
// user. Returns the path, or "" if cancelled.
func (s *AppService) ExportAudit(q core.AuditQuery, title string) (string, error) {
	path, err := application.Get().Dialog.SaveFile().
		SetFilename("audit-" + time.Now().Format("20060102-150405") + ".csv").
		SetMessage(title).
		PromptForSingleSelection()
	if err != nil || path == "" {
		return "", err
	}
	q.Limit = 5000
	var all []core.AuditEntry
	for {
		page, err := s.core.AuditList(q)
		if err != nil {
			return "", err
		}
		all = append(all, page...)
		if len(page) < q.Limit {
			break
		}
		q.Before = page[len(page)-1].ID
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	_ = w.Write([]string{"id", "time", "actor", "server", "action", "target", "detail", "ok", "error", "hash"})
	for _, e := range all {
		_ = w.Write([]string{strconv.FormatInt(e.ID, 10), time.UnixMilli(e.TS).Format(time.RFC3339), e.Actor, e.ServerName,
			e.Action, e.Target, csvSafe(e.Detail), strconv.FormatBool(e.OK), csvSafe(e.Error), e.Hash})
	}
	w.Flush()
	return path, w.Error()
}

// csvSafe defuses spreadsheet formula injection.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func (s *AppService) Events(q core.EventQuery) ([]core.Event, error) { return s.core.Events(q) }

func (s *AppService) Jobs(server string) []core.JobInfo { return s.core.Jobs.List(server) }

// Job returns a job including its output so far.
func (s *AppService) Job(id string) (core.JobInfo, bool) {
	j, ok := s.core.Jobs.Get(id)
	if !ok {
		return core.JobInfo{}, false
	}
	return j.Info(), true
}

func (s *AppService) CancelJob(id string) { s.core.Jobs.Cancel(id) }
