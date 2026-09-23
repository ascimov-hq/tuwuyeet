package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClassifyReply(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		ok    bool
		count int
	}{
		{"prune success", "Successfully deleted 12 rooms from our database.", true, 12},
		{"media success", "Deleted 340 total files.", true, 340},
		{"media nothing to do", "Command failed with error:\n```\nDatabase(\"Did not found any eligible MXCs to delete.\")\n```", true, 0},
		{"command error", "Command failed with error:\n```\nboom\n```", false, 0},
		{"command error after logs", "| level | message |\n| info | x |\nCommand failed with error:\n```\nboom\n```", false, 0},
		{"panic", "Panic occurred while processing command:\n```\nx\n```", false, 0},
		{"clap error", "error: unexpected argument '--bogus' found", false, 0},
		{"pdu failure", "Failed to build and append admin room PDU", false, 0},
		{"other success", "Done.", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := classifyReply(c.body)
			if o.OK != c.ok || o.Count != c.count {
				t.Fatalf("got ok=%v count=%d, want ok=%v count=%d", o.OK, o.Count, c.ok, c.count)
			}
		})
	}
}

func hourBuckets(start time.Time, hours int, load func(time.Time) float64) []Bucket {
	var out []Bucket
	for i := 0; i < hours; i++ {
		t := start.Add(time.Duration(i) * time.Hour)
		out = append(out, Bucket{Hour: t.Unix(), Sum: load(t), N: 1})
	}
	return out
}

var monday = time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)

func TestQuietestDaily(t *testing.T) {
	b := hourBuckets(monday, 48, func(t time.Time) float64 {
		if t.Hour() == 3 {
			return 2
		}
		return 50
	})
	s, ok := quietest(b, false)
	if !ok || s.Hour != 3 || s.Weekday != -1 {
		t.Fatalf("got %+v ok=%v", s, ok)
	}
}

func TestQuietestNeedsFullCoverage(t *testing.T) {
	if _, ok := quietest(hourBuckets(monday, 23, func(time.Time) float64 { return 1 }), false); ok {
		t.Fatal("daily profile with 23 hours should be incomplete")
	}
	if _, ok := quietest(hourBuckets(monday, window-1, func(time.Time) float64 { return 1 }), true); ok {
		t.Fatal("weekly profile with 167 hours should be incomplete")
	}
}

func TestQuietestWeekly(t *testing.T) {
	b := hourBuckets(monday, window, func(t time.Time) float64 {
		if t.Weekday() == time.Saturday && t.Hour() == 5 {
			return 1
		}
		return 40
	})
	s, ok := quietest(b, true)
	if !ok || s.Weekday != time.Saturday || s.Hour != 5 {
		t.Fatalf("got %+v ok=%v", s, ok)
	}
}

func TestNextAtWeeklyWraps(t *testing.T) {
	from := time.Date(2026, 9, 20, 5, 0, 0, 0, time.UTC)
	got := nextAt(from, time.UTC, time.Sunday, 4, 30)
	want := time.Date(2026, 9, 27, 4, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got = nextAt(from, time.UTC, -1, 4, 30)
	want = time.Date(2026, 9, 21, 4, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("daily got %v want %v", got, want)
	}
}

func testJobs(t *testing.T) []*Job {
	t.Helper()
	cfg := defaultConfig()
	cfg.ServerName = "example.org"
	return buildJobs(cfg)
}

func TestPlanFallbackUntilEnoughData(t *testing.T) {
	j := testJobs(t)[0]
	now := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	p := j.Plan(now, time.Time{}, time.UTC, nil)
	if p.Adaptive || !p.Next.Equal(time.Date(2026, 9, 21, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %+v", p)
	}
}

func TestPlanAdaptiveKeepsMinimumGap(t *testing.T) {
	j := testJobs(t)[0]
	b := hourBuckets(monday, 48, func(t time.Time) float64 {
		if t.Hour() == 5 {
			return 1
		}
		return 30
	})
	last := time.Date(2026, 9, 20, 3, 0, 0, 0, time.UTC)
	now := last.Add(time.Minute)
	p := j.Plan(now, last, time.UTC, b)
	want := time.Date(2026, 9, 21, 5, 0, 0, 0, time.UTC)
	if !p.Adaptive || !p.Next.Equal(want) {
		t.Fatalf("got %+v, want next %v", p, want)
	}
}

func TestPlanNotAdaptiveWhenDisabled(t *testing.T) {
	j := testJobs(t)[0]
	j.Cfg.Adaptive = false
	b := hourBuckets(monday, 48, func(time.Time) float64 { return 1 })
	if p := j.Plan(monday.Add(49*time.Hour), time.Time{}, time.UTC, b); p.Adaptive {
		t.Fatal("adaptive: false must use fallback")
	}
}

func TestCommands(t *testing.T) {
	cfg := defaultConfig()
	cfg.Jobs.PruneRooms.Force = true
	cfg.Jobs.ExpireMedia.OlderThan = "14d"
	cfg.Jobs.ExpireMedia.IncludeLocal = true
	jobs := buildJobs(cfg)
	if jobs[0].Command != "!admin rooms prune-empty --force" {
		t.Fatal(jobs[0].Command)
	}
	if jobs[1].Command != "!admin media delete-range 14d --older-than --yes-i-want-to-delete-local-media" {
		t.Fatal(jobs[1].Command)
	}
}

func TestProcStat(t *testing.T) {
	f, err := os.Open("testdata/proc_stat")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := parseProcStat(f)
	if err != nil {
		t.Fatal(err)
	}
	if got.total != 4705+356+584+3699+23+23 || got.idle != 3699+23 {
		t.Fatalf("got %+v", got)
	}
	pct, ok := busyPercent(cpuTimes{idle: 100, total: 200}, cpuTimes{idle: 150, total: 400})
	if !ok || pct != 75 {
		t.Fatalf("got %v %v", pct, ok)
	}
	if _, ok := busyPercent(got, got); ok {
		t.Fatal("zero delta must be skipped")
	}
}

func TestCPUStoreRollsAndTrims(t *testing.T) {
	s := LoadCPU(filepath.Join(t.TempDir(), "cpu.json"))
	if s.Add(monday, 10) {
		t.Fatal("first sample must not report a rollover")
	}
	s.Add(monday.Add(30*time.Minute), 20)
	if !s.Add(monday.Add(time.Hour), 5) {
		t.Fatal("new hour must report a rollover")
	}
	if got := s.Snapshot()[0].Avg(); got != 15 {
		t.Fatalf("avg %v", got)
	}
	s.Add(monday.Add(time.Duration(window+1)*time.Hour), 1)
	for _, b := range s.Snapshot() {
		if b.Hour == monday.Unix() {
			t.Fatal("bucket older than 7 days was kept")
		}
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if n := len(LoadCPU(s.path).Snapshot()); n != len(s.Snapshot()) {
		t.Fatalf("reloaded %d buckets", n)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("server_name: example.org\ntimezone: Europe/Berlin\njobs:\n  expire_media:\n    older_than: 14d\n"), 0o644)
	c, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Jobs.PruneRooms.Enabled || c.Jobs.PruneRooms.FallbackTime != "04:00" || c.Jobs.ExpireMedia.OlderThan != "14d" || c.loc.String() != "Europe/Berlin" {
		t.Fatalf("got %+v", c)
	}
	os.WriteFile(path, []byte("server_name: example.org\njobs:\n  expire_media:\n    fallback_time: Someday 04:00\n"), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("bad weekday accepted")
	}
}

type fakeHS struct {
	mu        sync.Mutex
	joined    bool
	sent      []string
	reply     string
	registers int
}

func (f *fakeHS) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_matrix/client/v3/register", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.registers++
		f.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"errcode":"M_USER_IN_USE","error":"taken"}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/directory/room/{alias}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("alias") != "#admins:example.org" {
			t.Errorf("alias %q", r.PathValue("alias"))
		}
		w.Write([]byte(`{"room_id":"!admin:example.org"}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/joined_rooms", func(w http.ResponseWriter, r *http.Request) {
		if f.joined {
			w.Write([]byte(`{"joined_rooms":["!admin:example.org"]}`))
			return
		}
		w.Write([]byte(`{"joined_rooms":[]}`))
	})
	mux.HandleFunc("PUT /_matrix/client/v3/rooms/{room}/send/m.room.message/{txn}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer astoken" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sent = append(f.sent, body["body"])
		f.mu.Unlock()
		w.Write([]byte(`{"event_id":"$cmd"}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/{room}/messages", func(w http.ResponseWriter, r *http.Request) {
		chunk := []map[string]any{
			{"type": "m.room.message", "sender": "@tuwuyeet:example.org", "content": map[string]any{"body": "!admin x"}},
			{"type": "m.room.message", "sender": "@conduit:example.org", "content": map[string]any{
				"body": "unrelated", "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$other"}}}},
			{"type": "m.room.message", "sender": "@conduit:example.org", "content": map[string]any{
				"body": f.reply, "m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$cmd"}}}},
		}
		json.NewEncoder(w).Encode(map[string]any{"chunk": chunk})
	})
	return mux
}

func TestRunJobAgainstFakeHomeserver(t *testing.T) {
	hs := &fakeHS{joined: true, reply: "Successfully deleted 4 rooms from our database."}
	srv := httptest.NewServer(hs.handler(t))
	defer srv.Close()
	cfg := defaultConfig()
	cfg.ServerName = "example.org"
	cfg.Homeserver = srv.URL
	client := NewClient(srv.URL, "example.org", Registration{ASToken: "astoken", SenderLocalpart: "tuwuyeet"})
	res, err := RunJob(context.Background(), cfg, client, buildJobs(cfg)[0])
	if err != nil || !res.OK || res.Count != 4 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if len(hs.sent) != 1 || hs.sent[0] != "!admin rooms prune-empty" || hs.registers != 1 {
		t.Fatalf("sent %v registers %d", hs.sent, hs.registers)
	}
}

func TestRunJobNotAdmin(t *testing.T) {
	hs := &fakeHS{joined: false}
	srv := httptest.NewServer(hs.handler(t))
	defer srv.Close()
	cfg := defaultConfig()
	cfg.ServerName = "example.org"
	client := NewClient(srv.URL, "example.org", Registration{ASToken: "astoken", SenderLocalpart: "tuwuyeet"})
	_, err := RunJob(context.Background(), cfg, client, buildJobs(cfg)[0])
	if err == nil || !strings.Contains(err.Error(), "not joined to the admin room") || len(hs.sent) != 0 {
		t.Fatalf("err %v sent %v", err, hs.sent)
	}
}

func TestPageRenders(t *testing.T) {
	cfg := defaultConfig()
	cfg.ServerName = "example.org"
	cfg.loc = time.UTC
	now := monday.Add(time.Duration(window) * time.Hour)
	cpu := LoadCPU(filepath.Join(t.TempDir(), "cpu.json"))
	for _, b := range hourBuckets(monday, window, func(t time.Time) float64 { return float64(t.Hour()) }) {
		cpu.Add(b.Start(), b.Sum)
	}
	s := NewScheduler(cfg, nil, cpu, buildJobs(cfg), filepath.Join(t.TempDir(), "state.json"))
	d := buildPage(cpu.Snapshot(), s.Status(now), time.UTC, now)
	var sb strings.Builder
	if err := page.Execute(&sb, d); err != nil {
		t.Fatal(err)
	}
	html := sb.String()
	for _, want := range []string{"prune-rooms", "expire-media", "quietest slot from CPU data", `stroke="#0f766e"`, `stroke="#c2410c"`} {
		if !strings.Contains(html, want) {
			t.Errorf("page missing %q", want)
		}
	}
}
