package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"adbtest/internal/store"
)

const defaultLimit = 200

// Server serves the HTTP dashboard.
type Server struct {
	store *store.Store
	tmpl  *template.Template
}

// NewServer creates a new Server.
func NewServer(s *store.Store) *Server {
	srv := &Server{store: s}
	srv.tmpl = template.Must(template.New("dashboard").Funcs(template.FuncMap{
		"fmtSecs": func(secs float64) string {
			if secs <= 0 {
				return "—"
			}
			s := int(secs)
			if s < 60 {
				return strconv.Itoa(s) + "s"
			}
			return strconv.Itoa(s/60) + "m " + strconv.Itoa(s%60) + "s"
		},
		"bootTime": func(r store.Run) string {
			if !r.BootOK {
				return "—"
			}
			secs := int(r.BootSeconds)
			if secs < 60 {
				return strconv.Itoa(secs) + "s"
			}
			return strconv.Itoa(secs/60) + "m " + strconv.Itoa(secs%60) + "s"
		},
		"sub":          func(a, b float64) float64 { return a - b },
		"printf":       fmt.Sprintf,
		"limitOptions": func() []int { return []int{50, 100, 200, 500} },
	}).Parse(dashboardHTML))
	return srv
}

// RegisterRoutes attaches routes to mux (pass nil for http.DefaultServeMux).
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/runs", s.handleAPIRuns)
}

// ServeAPKDir registers a file server for the APK directory at /apk/.
// Appium containers running with --network=host can fetch the APK via
// http://localhost:<port>/apk/<filename> without any volume mounts.
func (s *Server) ServeAPKDir(mux *http.ServeMux, dir string) {
	mux.Handle("/apk/", http.StripPrefix("/apk/", http.FileServer(http.Dir(dir))))
	log.Printf("[apk] serving %s at /apk/", dir)
}

type dashboardData struct {
	Runs    []store.Run
	Stats   []store.DeviceStats
	Devices []string
	Serial  string
	Limit   int
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	serial := r.URL.Query().Get("serial")
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	runs, err := s.store.List(serial, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devices, err := s.store.Devices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats, err := s.store.Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, dashboardData{
		Runs:    runs,
		Stats:   stats,
		Devices: devices,
		Serial:  serial,
		Limit:   limit,
	}); err != nil {
		log.Printf("[web] template error: %v", err)
	}
}

func (s *Server) handleAPIRuns(w http.ResponseWriter, r *http.Request) {
	serial := r.URL.Query().Get("serial")
	limit := defaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := s.store.List(serial, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runs)
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="60">
<title>adbtest — test results</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f1117;color:#e2e8f0;min-height:100vh}
header{background:#1a1d27;border-bottom:1px solid #2d3148;padding:16px 24px;display:flex;align-items:center;gap:16px}
header h1{font-size:1.2rem;font-weight:600;color:#a5b4fc}
header span{font-size:.8rem;color:#64748b}
.toolbar{padding:16px 24px;display:flex;gap:12px;align-items:center;flex-wrap:wrap}
select,button{background:#1e2235;color:#e2e8f0;border:1px solid #2d3148;border-radius:6px;padding:6px 12px;font-size:.85rem;cursor:pointer}
select:focus,button:focus{outline:2px solid #a5b4fc;outline-offset:2px}
button{background:#3730a3;border-color:#4338ca}
button:hover{background:#4338ca}
.count{font-size:.8rem;color:#64748b;margin-left:auto}
table{width:100%;border-collapse:collapse}
th{background:#1a1d27;color:#94a3b8;font-size:.75rem;text-transform:uppercase;letter-spacing:.05em;padding:10px 16px;text-align:left;position:sticky;top:0;border-bottom:1px solid #2d3148}
td{padding:10px 16px;border-bottom:1px solid #1a1d27;font-size:.875rem;vertical-align:middle}
tr:hover td{background:#1a1d27}
.badge{display:inline-block;padding:2px 10px;border-radius:12px;font-size:.75rem;font-weight:600;letter-spacing:.03em}
.pass{background:#14532d;color:#86efac}
.fail{background:#450a0a;color:#fca5a5}
.na{background:#1e293b;color:#64748b}
.mono{font-family:"SF Mono",Menlo,Consolas,monospace;font-size:.8rem}
.device{font-size:.8rem;color:#94a3b8}
.boot-ok{color:#86efac}
.boot-fail{color:#f87171}
.empty{text-align:center;padding:64px;color:#64748b}
</style>
</head>
<body>
<header>
  <h1>📱 adbtest</h1>
  <span>auto-refresh every 60s</span>
</header>

<div class="toolbar">
  <form method="get" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">
    <select name="serial" onchange="this.form.submit()">
      <option value="" {{if eq .Serial ""}}selected{{end}}>All devices</option>
      {{range .Devices}}
      <option value="{{.}}" {{if eq . $.Serial}}selected{{end}}>{{.}}</option>
      {{end}}
    </select>
    <select name="limit" onchange="this.form.submit()">
      {{range $n := limitOptions}}
      <option value="{{$n}}" {{if eq $n $.Limit}}selected{{end}}>Last {{$n}}</option>
      {{end}}
    </select>
    {{if .Serial}}<a href="/" style="color:#a5b4fc;font-size:.85rem;text-decoration:none">✕ clear filter</a>{{end}}
  </form>
  <span class="count">{{len .Runs}} rows</span>
</div>

{{if .Stats}}
<div style="padding:0 24px 24px">
<h2 style="font-size:.85rem;color:#64748b;text-transform:uppercase;letter-spacing:.07em;margin-bottom:12px">Device summary</h2>
<div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:12px">
{{range .Stats}}
<div style="background:#1a1d27;border:1px solid #2d3148;border-radius:10px;padding:16px">
  <div style="display:flex;justify-content:space-between;align-items:flex-start;margin-bottom:14px">
    <div>
      <div class="mono" style="font-size:.85rem;color:#e2e8f0">{{.Serial}}</div>
      {{if .Model}}<div style="font-size:.75rem;color:#64748b;margin-top:2px">{{.Model}}</div>{{end}}
    </div>
    <div style="text-align:right">
      {{if eq .FailedRuns 0}}
        <span class="badge pass">{{.TotalRuns}} runs</span>
      {{else}}
        <span class="badge fail">{{.FailedRuns}}/{{.TotalRuns}} failed</span>
      {{end}}
    </div>
  </div>
  <div style="display:grid;grid-template-columns:1fr 1fr;gap:8px;margin-bottom:12px">
    <div style="background:#0f1117;border-radius:6px;padding:10px">
      <div style="font-size:.7rem;color:#64748b;margin-bottom:4px">TESTS FAILED</div>
      <div style="font-size:1.1rem;font-weight:600;color:{{if gt .TotalFail 0}}#f87171{{else}}#86efac{{end}}">
        {{.TotalFail}}<span style="font-size:.75rem;font-weight:400;color:#64748b"> / {{.TotalTests}}</span>
      </div>
    </div>
    <div style="background:#0f1117;border-radius:6px;padding:10px">
      <div style="font-size:.7rem;color:#64748b;margin-bottom:4px">PASS RATE</div>
      <div style="font-size:1.1rem;font-weight:600;color:{{if lt (printf "%.0f" .PassRate) "80"}}#f87171{{else if lt (printf "%.0f" .PassRate) "100"}}#fbbf24{{else}}#86efac{{end}}">
        {{printf "%.0f" .PassRate}}%
      </div>
    </div>
  </div>
  <div style="background:#0f1117;border-radius:6px;padding:10px">
    <div style="font-size:.7rem;color:#64748b;margin-bottom:6px">REBOOT TIME</div>
    <div style="display:flex;justify-content:space-between;font-size:.8rem">
      <span style="color:#64748b">avg <span style="color:#94a3b8;font-weight:600">{{fmtSecs .AvgBoot}}</span></span>
      <span style="color:#64748b">min <span style="color:#86efac;font-weight:600">{{fmtSecs .MinBoot}}</span></span>
      <span style="color:#64748b">max <span style="color:#f87171;font-weight:600">{{fmtSecs .MaxBoot}}</span></span>
    </div>
  </div>
</div>
{{end}}
</div>
</div>
{{end}}

{{if not .Runs}}
<p class="empty">No test runs recorded yet.</p>
{{else}}
<div style="overflow-x:auto">
<table>
<thead>
<tr>
  <th>Time</th>
  <th>Device</th>
  <th>Result</th>
  <th>Passing</th>
  <th>Failing</th>
  <th>Pending</th>
  <th>Setup</th>
  <th>Tests</th>
  <th>Boot</th>
</tr>
</thead>
<tbody>
{{range .Runs}}
<tr>
  <td class="mono">{{.FinishedAt.Format "2006-01-02 15:04:05"}}</td>
  <td>
    <div class="mono">{{.Serial}}</div>
    {{if .Model}}<div class="device">{{.Model}}</div>{{end}}
  </td>
  <td>
    {{if not .Found}}
      <span class="badge na">N/A</span>
    {{else if gt .Failing 0}}
      <span class="badge fail">FAIL</span>
    {{else}}
      <span class="badge pass">PASS</span>
    {{end}}
  </td>
  <td style="color:#86efac">{{if .Found}}{{.Passing}}{{else}}—{{end}}</td>
  <td style="color:{{if gt .Failing 0}}#f87171{{else}}#64748b{{end}}">{{if .Found}}{{.Failing}}{{else}}—{{end}}</td>
  <td style="color:#64748b">{{if .Found}}{{.Pending}}{{else}}—{{end}}</td>
  <td style="color:#94a3b8" title="Session init + APK install">{{fmtSecs (sub .TotalSeconds .TestSeconds)}}</td>
  <td style="color:#94a3b8" title="Actual test execution">{{fmtSecs .TestSeconds}}</td>
  <td class="{{if .BootOK}}boot-ok{{else}}boot-fail{{end}}">{{bootTime .}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>
{{end}}
</body>
</html>`
