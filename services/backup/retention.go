package backup

import (
	"context"
	"sort"
	"strings"
	"time"

	"server-manager/internal/core"
)

// backupSet is a data object and its manifest.
type backupSet struct {
	Name        string // data object name
	Time        time.Time
	Size        int64
	HasData     bool
	HasManifest bool
}

func (b backupSet) complete() bool { return b.HasData && b.HasManifest }

// groupObjects pairs data objects with their manifests, newest first.
// Unrelated files (.part, foreign files) are ignored.
func groupObjects(list []objInfo) []backupSet {
	sets := map[string]*backupSet{}
	get := func(name string) *backupSet {
		if s, ok := sets[name]; ok {
			return s
		}
		t, ok := parseObjectName(name)
		if !ok {
			return nil
		}
		s := &backupSet{Name: name, Time: t}
		sets[name] = s
		return s
	}
	for _, o := range list {
		if strings.HasSuffix(o.Name, manifestSuffix) {
			if s := get(strings.TrimSuffix(o.Name, manifestSuffix)); s != nil {
				s.HasManifest = true
			}
			continue
		}
		if s := get(o.Name); s != nil {
			s.HasData = true
			s.Size = o.Size
		}
	}
	out := make([]backupSet, 0, len(sets))
	for _, s := range sets {
		out = append(out, *s)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Time.Equal(out[b].Time) {
			return out[a].Time.After(out[b].Time)
		}
		return out[a].Name > out[b].Name
	})
	return out
}

// incompleteGrace is how old an incomplete set (data without manifest or
// vice versa) must be before retention removes it.
const incompleteGrace = 24 * time.Hour

// selectForDeletion applies a retention policy to sets (newest first).
// KeepLast and MaxAgeDays are both limits: a complete backup is deleted
// when it is beyond the newest KeepLast or older than MaxAgeDays. The newest
// complete backup is never deleted. Incomplete sets are removed once older
// than a day. With no limit configured nothing is deleted.
func selectForDeletion(sets []backupSet, pol Retention, now time.Time) []backupSet {
	if pol.KeepLast <= 0 && pol.MaxAgeDays <= 0 {
		return nil
	}
	var del []backupSet
	idx := 0
	for _, s := range sets {
		if !s.complete() {
			if now.Sub(s.Time) > incompleteGrace {
				del = append(del, s)
			}
			continue
		}
		i := idx
		idx++
		if i == 0 {
			continue // never the newest successful backup
		}
		if pol.KeepLast > 0 && i >= pol.KeepLast {
			del = append(del, s)
			continue
		}
		if pol.MaxAgeDays > 0 && now.Sub(s.Time) > time.Duration(pol.MaxAgeDays)*24*time.Hour {
			del = append(del, s)
		}
	}
	return del
}

// applyRetention deletes backups beyond the job's policy. Each deletion is
// logged in the job output and audited.
func (s *BackupService) applyRetention(ctx context.Context, j *core.Job, job *Job, sk sink, now time.Time) (int, error) {
	list, err := sk.List(ctx, job.Folder)
	if err != nil {
		return 0, err
	}
	sets := groupObjects(list)
	del := selectForDeletion(sets, job.Retention, now)
	if len(del) == 0 {
		j.Logf("Nothing to delete (%d backups kept)", countComplete(sets))
		return 0, nil
	}
	n := 0
	var firstErr error
	for _, b := range del {
		key := job.Folder + "/" + b.Name
		var err error
		if b.HasData {
			err = sk.Remove(ctx, key)
		}
		if err == nil && b.HasManifest {
			err = sk.Remove(ctx, key+manifestSuffix)
		}
		s.core.Audit(job.Server, "backup.retention.delete", sk.Describe(key), job.Name, err)
		if err != nil {
			j.Logf("\x1b[33m  could not delete %s: %s\x1b[0m", b.Name, errText(err))
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
		j.Logf("  deleted %s (%s)", b.Name, b.Time.Local().Format("2006-01-02 15:04"))
	}
	j.Logf("Deleted %d old backup(s); %d kept", n, countComplete(sets)-n)
	return n, firstErr
}

func countComplete(sets []backupSet) int {
	n := 0
	for _, s := range sets {
		if s.complete() {
			n++
		}
	}
	return n
}
