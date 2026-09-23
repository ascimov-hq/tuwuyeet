package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const window = 7 * 24

type Bucket struct {
	Hour int64   `json:"hour"`
	Sum  float64 `json:"sum"`
	N    int     `json:"n"`
}

func (b Bucket) Avg() float64 {
	if b.N == 0 {
		return 0
	}
	return b.Sum / float64(b.N)
}

func (b Bucket) Start() time.Time { return time.Unix(b.Hour, 0).UTC() }

type CPUStore struct {
	mu      sync.Mutex
	path    string
	buckets []Bucket
}

func LoadCPU(path string) *CPUStore {
	s := &CPUStore{path: path}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &s.buckets); err != nil {
			slog.Warn("ignoring unreadable cpu history", "path", path, "err", err)
			s.buckets = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("cannot read cpu history", "path", path, "err", err)
	}
	return s
}

func (s *CPUStore) Add(t time.Time, pct float64) (rolled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hour := t.UTC().Truncate(time.Hour).Unix()
	if n := len(s.buckets); n > 0 && s.buckets[n-1].Hour == hour {
		s.buckets[n-1].Sum += pct
		s.buckets[n-1].N++
		return false
	}
	s.buckets = append(s.buckets, Bucket{Hour: hour, Sum: pct, N: 1})
	s.trim(hour)
	return len(s.buckets) > 1
}

func (s *CPUStore) trim(currentHour int64) {
	cutoff := currentHour - int64(window)*3600
	i := 0
	for i < len(s.buckets) && s.buckets[i].Hour <= cutoff {
		i++
	}
	s.buckets = append([]Bucket(nil), s.buckets[i:]...)
}

func (s *CPUStore) Snapshot() []Bucket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Bucket(nil), s.buckets...)
}

func (s *CPUStore) Save() error {
	s.mu.Lock()
	raw, err := json.Marshal(s.buckets)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return writeAtomic(s.path, raw)
}

func writeAtomic(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

type cpuTimes struct{ idle, total uint64 }

func parseProcStat(r io.Reader) (cpuTimes, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var vals []uint64
		for _, f := range fields[1:min(len(fields), 9)] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("bad /proc/stat field %q: %w", f, err)
			}
			vals = append(vals, v)
		}
		var t cpuTimes
		for i, v := range vals {
			t.total += v
			if i == 3 || i == 4 {
				t.idle += v
			}
		}
		return t, nil
	}
	if err := sc.Err(); err != nil {
		return cpuTimes{}, err
	}
	return cpuTimes{}, errors.New("no aggregate cpu line in /proc/stat")
}

func busyPercent(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0, false
	}
	dt := float64(cur.total - prev.total)
	di := float64(cur.idle - prev.idle)
	return 100 * (dt - di) / dt, true
}

func readProcStat(path string) (cpuTimes, error) {
	f, err := os.Open(path)
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	return parseProcStat(f)
}

func sampleCPU(ctx context.Context, store *CPUStore, statPath string, every time.Duration) {
	prev, err := readProcStat(statPath)
	if err != nil {
		slog.Error("cpu sampling disabled", "err", err)
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			cur, err := readProcStat(statPath)
			if err != nil {
				slog.Warn("cpu sample failed", "err", err)
				continue
			}
			pct, ok := busyPercent(prev, cur)
			prev = cur
			if !ok {
				continue
			}
			if store.Add(now, pct) {
				if err := store.Save(); err != nil {
					slog.Warn("saving cpu history failed", "err", err)
				}
			}
		}
	}
}
