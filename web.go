package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"time"
)

type cell struct {
	X, Y    int
	Fill    string
	Title   string
	Daily   bool
	Weekly  bool
	HasData bool
}

type row struct {
	Y     int
	Label string
}

type pageData struct {
	Cells    []cell
	Rows     []row
	Hours    []int
	Width    int
	Height   int
	Jobs     []JobStatus
	Max      float64
	Timezone string
	Now      string
	Legend   []string
}

const (
	cellW   = 26
	cellH   = 22
	labelW  = 92
	headerH = 20
)

var ramp = []string{"#e8eef7", "#c7d6ec", "#9db8de", "#6f95cc", "#4673b3", "#27528f", "#153766"}

func fillFor(v, peak float64) string {
	if peak <= 0 {
		return ramp[0]
	}
	i := int(math.Round(v / peak * float64(len(ramp)-1)))
	return ramp[min(max(i, 0), len(ramp)-1)]
}

func buildPage(buckets []Bucket, jobs []JobStatus, loc *time.Location, now time.Time) pageData {
	byHour := map[int64]Bucket{}
	peak := 0.0
	for _, b := range buckets {
		byHour[b.Hour] = b
		peak = math.Max(peak, b.Avg())
	}
	var daily, weekly *Slot
	for _, j := range jobs {
		if j.Slot == nil {
			continue
		}
		if j.Weekly {
			weekly = j.Slot
		} else {
			daily = j.Slot
		}
	}
	end := now.UTC().Truncate(time.Hour)
	start := end.Add(-time.Duration(window-1) * time.Hour)
	d := pageData{Jobs: jobs, Max: peak, Timezone: loc.String(), Now: now.In(loc).Format("Mon 2 Jan 15:04")}
	rowIndex := map[string]int{}
	for t := start; !t.After(end); t = t.Add(time.Hour) {
		lt := t.In(loc)
		key := lt.Format("2006-01-02")
		r, ok := rowIndex[key]
		if !ok {
			r = len(d.Rows)
			rowIndex[key] = r
			d.Rows = append(d.Rows, row{Y: headerH + r*cellH + cellH/2 + 4, Label: lt.Format("Mon 2 Jan")})
		}
		c := cell{X: labelW + lt.Hour()*cellW, Y: headerH + r*cellH, Fill: "#f4f4f4"}
		if b, ok := byHour[t.Unix()]; ok && b.N > 0 {
			c.HasData = true
			c.Fill = fillFor(b.Avg(), peak)
			c.Title = fmt.Sprintf("%s — %.1f%% CPU", lt.Format("Mon 2 Jan 15:04"), b.Avg())
		} else {
			c.Title = lt.Format("Mon 2 Jan 15:04") + " — no data"
		}
		c.Daily = daily != nil && t.Hour() == daily.Hour
		c.Weekly = weekly != nil && t.Hour() == weekly.Hour && t.Weekday() == weekly.Weekday
		d.Cells = append(d.Cells, c)
	}
	for h := 0; h < 24; h++ {
		d.Hours = append(d.Hours, h)
	}
	d.Width = labelW + 24*cellW + 2
	d.Height = headerH + len(d.Rows)*cellH + 2
	d.Legend = ramp
	return d
}

var page = template.Must(template.New("page").Funcs(template.FuncMap{
	"hx": func(h int) int { return labelW + h*cellW + cellW/2 },
}).Parse(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tuwuyeet</title>
<style>
body{font:14px/1.45 system-ui,-apple-system,sans-serif;margin:0;padding:24px 16px;background:#fbfbfa;color:#1d1d1f}
main{max-width:860px;margin:0 auto}
h1{font-size:20px;margin:0 0 4px}
.sub{color:#6b6b70;margin:0 0 20px}
.chart{overflow-x:auto;background:#fff;border:1px solid #e6e6e3;border-radius:8px;padding:12px}
svg text{font-size:11px;fill:#6b6b70}
.key{display:flex;gap:16px;flex-wrap:wrap;align-items:center;margin-top:10px;color:#6b6b70;font-size:12px}
.ramp{display:inline-flex;vertical-align:middle;margin:0 6px}
.ramp i{width:14px;height:10px;display:inline-block}
.sw{display:inline-block;width:12px;height:10px;vertical-align:middle;margin-right:4px}
table{border-collapse:collapse;width:100%;margin-top:20px;background:#fff;border:1px solid #e6e6e3;border-radius:8px;overflow:hidden}
th,td{text-align:left;padding:8px 10px;border-top:1px solid #efefec;vertical-align:top}
th{background:#f6f6f4;border-top:0;font-weight:600}
code{font-size:12px}
.ok{color:#1f7a3d}.bad{color:#b3261e}.muted{color:#8a8a8f}
</style>
<main>
<h1>tuwuyeet</h1>
<p class="sub">Box CPU, hourly average, last 7 days · {{.Timezone}} · now {{.Now}}</p>
<div class="chart">
<svg width="{{.Width}}" height="{{.Height}}" role="img" aria-label="CPU heatmap">
{{range $h := .Hours}}<text x="{{hx $h}}" y="13" text-anchor="middle">{{printf "%02d" $h}}</text>{{end}}
{{range .Rows}}<text x="0" y="{{.Y}}">{{.Label}}</text>{{end}}
{{range .Cells}}<rect x="{{.X}}" y="{{.Y}}" width="25" height="21" rx="2" fill="{{.Fill}}"{{if .Weekly}} stroke="#c2410c" stroke-width="2.5"{{else if .Daily}} stroke="#0f766e" stroke-width="2"{{end}}><title>{{.Title}}</title></rect>{{end}}
</svg>
<div class="key">
<span>low<span class="ramp">{{range .Legend}}<i style="background:{{.}}"></i>{{end}}</span>high ({{printf "%.0f" .Max}}%)</span>
<span><span class="sw" style="border:2px solid #0f766e"></span>prune-rooms slot</span>
<span><span class="sw" style="border:2px solid #c2410c"></span>expire-media slot</span>
</div>
</div>
<table>
<tr><th>Job</th><th>Next run</th><th>Last result</th></tr>
{{range .Jobs}}<tr>
<td><b>{{.Name}}</b><br><code>{{.Command}}</code></td>
<td>{{if .Enabled}}{{.Next}}<br><span class="muted">{{if .Adaptive}}quietest slot from CPU data{{else}}fallback time{{end}}</span>{{else}}<span class="muted">disabled</span>{{end}}</td>
<td>{{with .LastResult}}<span class="{{if .OK}}ok{{else}}bad{{end}}">{{if .OK}}ok{{else}}failed{{end}}</span> · {{.At.Format "Mon 2 Jan 15:04"}}<br>{{.Summary}}{{else}}<span class="muted">never run</span>{{end}}</td>
</tr>{{end}}
</table>
</main>
`))

func serveWeb(s *Scheduler, cpu *CPUStore, loc *time.Location) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		d := buildPage(cpu.Snapshot(), s.Status(now), loc, now)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(w, d); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		type point struct {
			Hour time.Time `json:"hour"`
			CPU  float64   `json:"cpu"`
		}
		var pts []point
		for _, b := range cpu.Snapshot() {
			pts = append(pts, point{Hour: b.Start(), CPU: math.Round(b.Avg()*10) / 10})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jobs": s.Status(time.Now()), "cpu": pts})
	})
	return mux
}
