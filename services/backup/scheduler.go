package backup

import (
	"log"
	"sync"
	"time"

	"server-manager/internal/db"
)

// scheduler fires due backup jobs. For every job it remembers the time up
// to which activations have been handled ("from"); a job is due when its
// cron schedule has an activation in (from, now]. Several missed
// activations are coalesced into one run. At startup, from is the last time
// the scheduler was alive but at most 24 h ago, so a schedule missed while
// the app was closed runs once if it was missed by less than a day.
type scheduler struct {
	s    *BackupService
	mu   sync.Mutex
	from map[string]time.Time
	stop chan struct{}
	done chan struct{}
	// lastAlive is when the previous app session last checked schedules.
	lastAlive time.Time
}

const missedWindow = 24 * time.Hour

type schedState struct {
	LastAlive int64 `json:"lastAlive"` // unix ms
}

func (s *BackupService) startScheduler(interval time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sched != nil {
		return
	}
	sc := &scheduler{s: s, from: map[string]time.Time{}, stop: make(chan struct{}), done: make(chan struct{})}
	var st schedState
	if err := s.core.DB.Get(nsState, "scheduler", &st); err == nil && st.LastAlive > 0 {
		sc.lastAlive = time.UnixMilli(st.LastAlive)
	}
	s.sched = sc
	go sc.loop(interval)
}

func (s *BackupService) stopScheduler() {
	s.mu.Lock()
	sc := s.sched
	s.sched = nil
	s.mu.Unlock()
	if sc == nil {
		return
	}
	close(sc.stop)
	<-sc.done
}

func (sc *scheduler) loop(interval time.Duration) {
	defer close(sc.done)
	// Let the app finish starting before the first check.
	select {
	case <-time.After(3 * time.Second):
	case <-sc.stop:
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		sc.tick(time.Now())
		select {
		case <-t.C:
		case <-sc.stop:
			return
		}
	}
}

// reset makes a (re)configured job count activations from now on.
func (sc *scheduler) reset(jobID string, now time.Time) {
	sc.mu.Lock()
	sc.from[jobID] = now
	sc.mu.Unlock()
}

// due returns the jobs to fire at now and advances their marks.
func (sc *scheduler) due(jobs []Job, now time.Time) []dueJob {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := []dueJob{}
	seen := map[string]bool{}
	for _, j := range jobs {
		seen[j.ID] = true
		if !j.Enabled || j.Schedule.Cron == "" {
			continue
		}
		sched, err := parseCron(j.Schedule.Cron)
		if err != nil {
			continue
		}
		from, ok := sc.from[j.ID]
		if !ok {
			from = now.Add(-missedWindow)
			if sc.lastAlive.After(from) {
				from = sc.lastAlive
			}
			if upd := time.UnixMilli(j.Updated); upd.After(from) {
				from = upd
			}
			if from.After(now) {
				from = now
			}
		}
		next := sched.Next(from)
		if next.IsZero() || next.After(now) {
			sc.from[j.ID] = from
			continue
		}
		trigger := "schedule"
		if now.Sub(next) > 2*time.Minute {
			trigger = "missed"
		}
		out = append(out, dueJob{job: j, trigger: trigger, at: next})
		sc.from[j.ID] = now
	}
	for id := range sc.from {
		if !seen[id] {
			delete(sc.from, id)
		}
	}
	return out
}

type dueJob struct {
	job     Job
	trigger string
	at      time.Time
}

func (sc *scheduler) tick(now time.Time) {
	jobs, err := db.List[Job](sc.s.core.DB, nsJob)
	if err != nil {
		log.Printf("[backup] scheduler: %v", err)
		return
	}
	for _, d := range sc.due(jobs, now) {
		if _, err := sc.s.core.Store.Get(d.job.Server); err != nil {
			continue // server profile deleted
		}
		if _, err := sc.s.startRun(d.job, d.trigger, "scheduler", ""); err != nil {
			log.Printf("[backup] scheduler: %s: %v", d.job.Name, err)
		}
	}
	_ = sc.s.core.DB.Put(nsState, "scheduler", schedState{LastAlive: now.UnixMilli()})
}

// nextRun is the job's next activation (zero when manual/disabled).
func nextRun(j *Job, now time.Time) time.Time {
	if !j.Enabled || j.Schedule.Cron == "" {
		return time.Time{}
	}
	sc, err := parseCron(j.Schedule.Cron)
	if err != nil {
		return time.Time{}
	}
	return sc.Next(now)
}
