package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

const (
	maxAttempts = 3
	retryDelay  = 15 * time.Minute
)

type Job struct {
	Name     string
	Every    time.Duration
	Weekly   bool
	Cfg      JobConfig
	Command  string
	fallback func(from time.Time, loc *time.Location) time.Time
}

func buildJobs(cfg Config) []*Job {
	prune := "!admin rooms prune-empty"
	if cfg.Jobs.PruneRooms.Force {
		prune += " --force"
	}
	media := "!admin media delete-range " + cfg.Jobs.ExpireMedia.OlderThan + " --older-than"
	if cfg.Jobs.ExpireMedia.IncludeLocal {
		media += " --yes-i-want-to-delete-local-media"
	}
	ph, pm, _ := parseDailyTime(cfg.Jobs.PruneRooms.FallbackTime)
	md, mh, mm, _ := parseWeeklyTime(cfg.Jobs.ExpireMedia.FallbackTime)
	return []*Job{
		{
			Name: "prune-rooms", Every: 24 * time.Hour, Cfg: cfg.Jobs.PruneRooms, Command: prune,
			fallback: func(from time.Time, loc *time.Location) time.Time { return nextAt(from, loc, -1, ph, pm) },
		},
		{
			Name: "expire-media", Every: 7 * 24 * time.Hour, Weekly: true, Cfg: cfg.Jobs.ExpireMedia, Command: media,
			fallback: func(from time.Time, loc *time.Location) time.Time { return nextAt(from, loc, md, mh, mm) },
		},
	}
}

func nextAt(from time.Time, loc *time.Location, day time.Weekday, hour, minute int) time.Time {
	f := from.In(loc)
	base := time.Date(f.Year(), f.Month(), f.Day(), hour, minute, 0, 0, loc)
	for i := 0; i <= 7; i++ {
		c := base.AddDate(0, 0, i)
		if c.Before(from) {
			continue
		}
		if day < 0 || c.Weekday() == day {
			return c
		}
	}
	return base.AddDate(0, 0, 7)
}

type Slot struct {
	Weekday time.Weekday
	Hour    int
}

func profile(buckets []Bucket, weekly bool) (avgs map[Slot]float64, complete bool) {
	sums := map[Slot]float64{}
	counts := map[Slot]int{}
	for _, b := range buckets {
		if b.N == 0 {
			continue
		}
		t := b.Start()
		s := Slot{Weekday: -1, Hour: t.Hour()}
		if weekly {
			s.Weekday = t.Weekday()
		}
		sums[s] += b.Avg()
		counts[s]++
	}
	avgs = map[Slot]float64{}
	for s, sum := range sums {
		avgs[s] = sum / float64(counts[s])
	}
	need := 24
	if weekly {
		need = window
	}
	return avgs, len(avgs) >= need
}

func quietest(buckets []Bucket, weekly bool) (Slot, bool) {
	avgs, complete := profile(buckets, weekly)
	if !complete {
		return Slot{}, false
	}
	var best Slot
	bestVal := -1.0
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			s := Slot{Weekday: time.Weekday(d), Hour: h}
			if !weekly {
				if d > 0 {
					break
				}
				s.Weekday = -1
			}
			v, ok := avgs[s]
			if ok && (bestVal < 0 || v < bestVal) {
				best, bestVal = s, v
			}
		}
	}
	return best, true
}

type Plan struct {
	Next     time.Time
	Adaptive bool
	Slot     *Slot
}

func (j *Job) Plan(now time.Time, last time.Time, loc *time.Location, buckets []Bucket) Plan {
	from := now
	if !last.IsZero() {
		if earliest := last.Add(j.Every / 2); earliest.After(from) {
			from = earliest
		}
	}
	if j.Cfg.Adaptive {
		if s, ok := quietest(buckets, j.Weekly); ok {
			return Plan{Next: nextAt(from, time.UTC, s.Weekday, s.Hour, 0), Adaptive: true, Slot: &s}
		}
	}
	return Plan{Next: j.fallback(from, loc)}
}

type Result struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Count   int       `json:"count"`
	Summary string    `json:"summary"`
}

type JobState struct {
	LastSuccess time.Time `json:"last_success"`
	RetryAt     time.Time `json:"retry_at"`
	Attempts    int       `json:"attempts"`
	LastResult  *Result   `json:"last_result,omitempty"`
}

type Scheduler struct {
	cfg    Config
	client *Client
	cpu    *CPUStore
	jobs   []*Job
	path   string

	mu    sync.Mutex
	state map[string]*JobState
}

func NewScheduler(cfg Config, client *Client, cpu *CPUStore, jobs []*Job, statePath string) *Scheduler {
	s := &Scheduler{cfg: cfg, client: client, cpu: cpu, jobs: jobs, path: statePath, state: map[string]*JobState{}}
	raw, err := os.ReadFile(statePath)
	if err == nil {
		if err := json.Unmarshal(raw, &s.state); err != nil {
			slog.Warn("ignoring unreadable scheduler state", "path", statePath, "err", err)
			s.state = map[string]*JobState{}
		}
	}
	for _, j := range jobs {
		if s.state[j.Name] == nil {
			s.state[j.Name] = &JobState{}
		}
	}
	return s
}

func (s *Scheduler) save() {
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err == nil {
		err = writeAtomic(s.path, raw)
	}
	if err != nil {
		slog.Warn("saving scheduler state failed", "err", err)
	}
}

func (s *Scheduler) due(j *Job, now time.Time) (Plan, bool) {
	st := s.state[j.Name]
	p := j.Plan(now, st.LastSuccess, s.cfg.loc, s.cpu.Snapshot())
	if !st.RetryAt.IsZero() && st.RetryAt.Before(p.Next) {
		p.Next = st.RetryAt
	}
	return p, !now.Before(p.Next)
}

func (s *Scheduler) Loop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for _, j := range s.jobs {
		if j.Cfg.Enabled {
			s.mu.Lock()
			p, _ := s.due(j, time.Now())
			s.mu.Unlock()
			slog.Info("job scheduled", "job", j.Name, "next", p.Next.In(s.cfg.loc).Format(time.RFC3339), "adaptive", p.Adaptive)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			for _, j := range s.jobs {
				if !j.Cfg.Enabled {
					continue
				}
				s.mu.Lock()
				_, ok := s.due(j, now)
				s.mu.Unlock()
				if ok {
					s.run(ctx, j)
				}
			}
		}
	}
}

func (s *Scheduler) run(ctx context.Context, j *Job) {
	res, err := RunJob(ctx, s.cfg, s.client, j)
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state[j.Name]
	st.LastResult = &res
	if err == nil && res.OK {
		st.LastSuccess, st.RetryAt, st.Attempts = res.At, time.Time{}, 0
	} else {
		st.Attempts++
		if st.Attempts < maxAttempts {
			st.RetryAt = time.Now().Add(retryDelay)
		} else {
			st.LastSuccess, st.RetryAt, st.Attempts = res.At, time.Time{}, 0
			slog.Error("job gave up until its next slot", "job", j.Name, "attempts", maxAttempts)
		}
	}
	s.save()
	p, _ := s.due(j, time.Now())
	slog.Info("job scheduled", "job", j.Name, "next", p.Next.In(s.cfg.loc).Format(time.RFC3339), "adaptive", p.Adaptive)
}

func RunJob(ctx context.Context, cfg Config, client *Client, j *Job) (Result, error) {
	start := time.Now()
	res := Result{At: start}
	if err := client.EnsureRegistered(ctx); err != nil {
		res.Summary = "register: " + err.Error()
		slog.Error("job failed", "job", j.Name, "err", res.Summary)
		return res, err
	}
	out, err := client.RunAdmin(ctx, cfg.ServerName, j.Command, cfg.ReplyTimeout.Duration)
	if err != nil {
		res.Summary = err.Error()
		if errors.Is(err, ErrNotAdmin) {
			slog.Error("job failed: not in admin room", "job", j.Name, "err", err)
		} else {
			slog.Error("job failed", "job", j.Name, "err", err)
		}
		return res, err
	}
	res.OK, res.Count, res.Summary = out.OK, out.Count, firstLine(out.Body)
	level := slog.LevelInfo
	if !out.OK {
		level = slog.LevelError
	}
	slog.Log(ctx, level, "job finished", "job", j.Name, "success", out.OK, "count", out.Count,
		"took", time.Since(start).Round(time.Millisecond).String(), "reply", out.Body)
	if !out.OK {
		return res, fmt.Errorf("tuwunel reported failure: %s", res.Summary)
	}
	return res, nil
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

type JobStatus struct {
	Name       string  `json:"name"`
	Enabled    bool    `json:"enabled"`
	Command    string  `json:"command"`
	Next       string  `json:"next,omitempty"`
	Adaptive   bool    `json:"adaptive"`
	Slot       *Slot   `json:"-"`
	Weekly     bool    `json:"weekly"`
	LastResult *Result `json:"last_result,omitempty"`
}

func (s *Scheduler) Status(now time.Time) []JobStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []JobStatus
	for _, j := range s.jobs {
		st := s.state[j.Name]
		js := JobStatus{Name: j.Name, Enabled: j.Cfg.Enabled, Command: j.Command, Weekly: j.Weekly, LastResult: st.LastResult}
		if j.Cfg.Enabled {
			p, _ := s.due(j, now)
			js.Next, js.Adaptive, js.Slot = p.Next.In(s.cfg.loc).Format(time.RFC3339), p.Adaptive, p.Slot
		}
		out = append(out, js)
	}
	return out
}
