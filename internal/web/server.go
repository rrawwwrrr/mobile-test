package web

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"adbtest/internal/store"
)

const defaultLimit = 200

// Hub broadcasts refresh signals to all connected SSE clients.
type Hub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

// NewHub creates a new Hub.
func NewHub() *Hub {
	return &Hub{clients: make(map[chan struct{}]struct{})}
}

func (h *Hub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

// Notify sends a refresh signal to all connected SSE clients.
func (h *Hub) Notify() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default: // drop if client is slow
		}
	}
}

// Server serves the HTTP dashboard.
type Server struct {
	store *store.Store
	hub   *Hub
	tmpl  *template.Template
}

// NewServer creates a new Server.
func NewServer(s *store.Store, hub *Hub) *Server {
	srv := &Server{store: s, hub: hub}
	srv.tmpl = template.Must(template.New("dashboard").Funcs(template.FuncMap{
		"fmtSecs": func(secs float64) string {
			if secs <= 0 {
				return "—"
			}
			s := int(secs)
			if s < 60 {
				return strconv.Itoa(s) + "с"
			}
			return strconv.Itoa(s/60) + "м " + strconv.Itoa(s%60) + "с"
		},
		"bootTime": func(r store.Run) string {
			if !r.BootOK {
				return "—"
			}
			secs := int(r.BootSeconds)
			if secs < 60 {
				return strconv.Itoa(secs) + "с"
			}
			return strconv.Itoa(secs/60) + "м " + strconv.Itoa(secs%60) + "с"
		},
		"sub":          func(a, b float64) float64 { return a - b },
		"printf":       fmt.Sprintf,
		"limitOptions": func() []int { return []int{50, 100, 200, 500} },
	}).Parse(dashboardHTML))
	return srv
}

// RegisterRoutes attaches routes to mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/runs", s.handleAPIRuns)
	mux.HandleFunc("/api/stats", s.handleAPIStats)
	mux.HandleFunc("/api/device-events", s.handleAPIDeviceEvents)
	mux.HandleFunc("/api/log", s.handleAPILog)
	mux.HandleFunc("/events", s.handleEvents)
}

// ServeAPKDir registers a file server for the APK directory at /apk/.
func (s *Server) ServeAPKDir(mux *http.ServeMux, dir string) {
	mux.Handle("/apk/", http.StripPrefix("/apk/", http.FileServer(http.Dir(dir))))
	log.Printf("[apk] serving %s at /apk/", dir)
}

// ServeLogsDir registers a file server for the logs directory at /logs/.
func (s *Server) ServeLogsDir(mux *http.ServeMux, dir string) {
	mux.Handle("/logs/", http.StripPrefix("/logs/", http.FileServer(http.Dir(dir))))
	log.Printf("[logs] serving %s at /logs/", dir)
}

// handleEvents streams Server-Sent Events refresh signals to the client.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)

	// Initial event confirms the connection is alive.
	fmt.Fprintf(w, "data: connected\n\n")
	flusher.Flush()

	for {
		select {
		case <-ch:
			fmt.Fprintf(w, "data: refresh\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// handleAPILog serves a single log file as plain text.
// Query params: id (run ID), type ("test" or "appium").
func (s *Server) handleAPILog(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	typ := r.URL.Query().Get("type")
	if id == "" || (typ != "test" && typ != "appium") {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Validate id is numeric to prevent path traversal.
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	path := filepath.Join("reports", "logs", id, typ+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

type dashboardData struct {
	Runs         []store.Run
	Stats        []store.DeviceStats
	DeviceEvents []store.DeviceEvent
	Devices      []string
	Serial       string
	Limit        int
	Period       string
}

// periodBounds converts a period string to from/to time bounds.
func periodBounds(period string) (from, to time.Time) {
	now := time.Now()
	today := now.Truncate(24 * time.Hour)
	switch period {
	case "today":
		from = today
	case "yesterday":
		from = today.AddDate(0, 0, -1)
		to = today
	case "7d":
		from = now.AddDate(0, 0, -7)
	case "30d":
		from = now.AddDate(0, 0, -30)
	}
	return
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	serial := q.Get("serial")
	period := q.Get("period")
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	from, to := periodBounds(period)

	runs, err := s.store.List(serial, limit, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	devices, err := s.store.Devices()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	stats, err := s.store.Stats(from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	deviceEvents, err := s.store.ListEvents(serial, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, dashboardData{
		Runs:         runs,
		Stats:        stats,
		DeviceEvents: deviceEvents,
		Devices:      devices,
		Serial:       serial,
		Limit:        limit,
		Period:       period,
	}); err != nil {
		log.Printf("[web] template error: %v", err)
	}
}

func (s *Server) handleAPIRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	serial := q.Get("serial")
	period := q.Get("period")
	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	from, to := periodBounds(period)
	runs, err := s.store.List(serial, limit, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(runs)
}

func (s *Server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	period := r.URL.Query().Get("period")
	from, to := periodBounds(period)
	stats, err := s.store.Stats(from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) handleAPIDeviceEvents(w http.ResponseWriter, r *http.Request) {
	serial := r.URL.Query().Get("serial")
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	events, err := s.store.ListEvents(serial, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>adbtest — результаты тестов</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:#0f1117;color:#e2e8f0;min-height:100vh}
header{background:#1a1d27;border-bottom:1px solid #2d3148;padding:16px 24px;display:flex;align-items:center;gap:16px}
header h1{font-size:1.2rem;font-weight:600;color:#a5b4fc}
.toolbar{padding:16px 24px;display:flex;gap:12px;align-items:center;flex-wrap:wrap}
select{background:#1e2235;color:#e2e8f0;border:1px solid #2d3148;border-radius:6px;padding:6px 12px;font-size:.85rem;cursor:pointer}
select:focus{outline:2px solid #a5b4fc;outline-offset:2px}
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
.log-btn{background:#1e2235;color:#a5b4fc;border:1px solid #3730a3;border-radius:4px;padding:3px 8px;font-size:.75rem;cursor:pointer;margin-right:4px;font-family:inherit}
.log-btn:hover{background:#3730a3}
.log-btn.ab{color:#94a3b8;border-color:#2d3148}
.log-btn.ab:hover{background:#2d3148}
.log-btn.sc{color:#fbbf24;border-color:#92400e}
.log-btn.sc:hover{background:#92400e}
.hist-btn{background:#1e2235;color:#c4b5fd;border:1px solid #4c1d95;border-radius:4px;padding:5px 10px;font-size:.75rem;cursor:pointer;font-family:inherit;margin-top:8px;width:100%;text-align:center}
.hist-btn:hover{background:#4c1d95}
/* Modal */
#modal{position:fixed;inset:0;background:rgba(0,0,0,.8);display:flex;align-items:center;justify-content:center;z-index:100;padding:20px}
#mbox{background:#1a1d27;border:1px solid #2d3148;border-radius:12px;width:100%;max-width:min(95vw,1600px);max-height:90vh;display:flex;flex-direction:column;overflow:hidden}
#mhdr{display:flex;align-items:center;justify-content:space-between;padding:14px 20px;border-bottom:1px solid #2d3148;flex-shrink:0;gap:12px}
#mtitle{font-size:.95rem;font-weight:600;color:#e2e8f0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.mtabs{display:flex;gap:6px;flex-shrink:0}
.mtab{background:transparent;border:1px solid #2d3148;color:#64748b;border-radius:6px;padding:4px 12px;font-size:.8rem;cursor:pointer;font-family:inherit}
.mtab.on{background:#3730a3;border-color:#4338ca;color:#e2e8f0}
.mclose{background:transparent;border:1px solid #2d3148;color:#64748b;border-radius:6px;padding:4px 10px;font-size:.8rem;cursor:pointer;font-family:inherit}
.mclose:hover{background:#450a0a;border-color:#ef4444;color:#fca5a5}
#mbody{flex:1;overflow:auto;padding:20px}
#mscr{max-width:100%;border-radius:8px;margin-bottom:16px;display:block;border:1px solid #2d3148}
#mpre{font-family:"SF Mono",Menlo,Consolas,monospace;font-size:.78rem;line-height:1.6;color:#94a3b8;white-space:pre-wrap;word-break:break-all;margin:0}
</style>
</head>
<body>
<header>
  <h1>📱 adbtest</h1>
  <span id="cst" style="font-size:.8rem;color:#64748b">⏳ подключение...</span>
</header>

<!-- Log modal -->
<div id="modal" style="display:none" onclick="if(event.target===this)closeModal()">
<div id="mbox">
  <div id="mhdr">
    <span id="mtitle">Лог</span>
    <div class="mtabs">
      <button id="btn-t" class="mtab on" onclick="switchLog('test')">тест</button>
      <button id="btn-a" class="mtab"    onclick="switchLog('appium')">appium</button>
      <button id="btn-s" class="mtab"    onclick="switchLog('screenshot')" style="display:none">📷 скрин</button>
      <button class="mclose" onclick="closeModal()">✕ закрыть</button>
    </div>
  </div>
  <div id="mbody">
    <img id="mscr" src="" alt="" onerror="this.style.display='none'">
    <pre id="mpre">Загрузка...</pre>
    <div id="mhist" style="display:none"></div>
  </div>
</div>
</div>

<div class="toolbar">
  <form method="get" style="display:flex;gap:8px;align-items:center;flex-wrap:wrap">
    <select name="serial" onchange="this.form.submit()">
      <option value="" {{if eq .Serial ""}}selected{{end}}>Все устройства</option>
      {{range .Devices}}
      <option value="{{.}}" {{if eq . $.Serial}}selected{{end}}>{{.}}</option>
      {{end}}
    </select>
    <select name="period" onchange="this.form.submit()">
      <option value=""          {{if eq .Period ""}}selected{{end}}>Всё время</option>
      <option value="today"     {{if eq .Period "today"}}selected{{end}}>Сегодня</option>
      <option value="yesterday" {{if eq .Period "yesterday"}}selected{{end}}>Вчера</option>
      <option value="7d"        {{if eq .Period "7d"}}selected{{end}}>7 дней</option>
      <option value="30d"       {{if eq .Period "30d"}}selected{{end}}>30 дней</option>
    </select>
    <select name="limit" onchange="this.form.submit()">
      {{range $n := limitOptions}}
      <option value="{{$n}}" {{if eq $n $.Limit}}selected{{end}}>Последние {{$n}}</option>
      {{end}}
    </select>
    {{if .Serial}}<a href="/" style="color:#a5b4fc;font-size:.85rem;text-decoration:none">✕ сбросить</a>{{end}}
  </form>
  <span class="count" id="rcount">{{len .Runs}} записей</span>
</div>

<div id="spanel"{{if not .Stats}} style="display:none"{{end}}>
<div style="padding:0 24px 24px">
<h2 style="font-size:.85rem;color:#64748b;text-transform:uppercase;letter-spacing:.07em;margin-bottom:12px">Сводка по устройствам</h2>
<div id="sgrid" style="display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:12px">
{{range .Stats}}
<div style="background:#1a1d27;border:1px solid #2d3148;border-radius:10px;padding:16px">
  <div style="display:flex;justify-content:space-between;align-items:flex-start;margin-bottom:14px">
    <div>
      <div class="mono" style="font-size:.85rem;color:#e2e8f0">{{.Serial}}</div>
      {{if .Model}}<div style="font-size:.75rem;color:#64748b;margin-top:2px">{{.Model}}</div>{{end}}
    </div>
    <div style="text-align:right">
      {{if eq .FailedRuns 0}}
        <span class="badge pass">{{.TotalRuns}} прогонов</span>
      {{else}}
        <span class="badge fail">{{.FailedRuns}}/{{.TotalRuns}} упало</span>
      {{end}}
    </div>
  </div>
  <div style="display:grid;grid-template-columns:1fr 1fr;gap:8px;margin-bottom:12px">
    <div style="background:#0f1117;border-radius:6px;padding:10px">
      <div style="font-size:.7rem;color:#64748b;margin-bottom:4px">УПАЛО ТЕСТОВ</div>
      <div style="font-size:1.1rem;font-weight:600;color:{{if gt .TotalFail 0}}#f87171{{else}}#86efac{{end}}">
        {{.TotalFail}}<span style="font-size:.75rem;font-weight:400;color:#64748b"> / {{.TotalTests}}</span>
      </div>
    </div>
    <div style="background:#0f1117;border-radius:6px;padding:10px">
      <div style="font-size:.7rem;color:#64748b;margin-bottom:4px">УСПЕШНОСТЬ</div>
      <div style="font-size:1.1rem;font-weight:600;color:{{if lt (printf "%.0f" .PassRate) "80"}}#f87171{{else if lt (printf "%.0f" .PassRate) "100"}}#fbbf24{{else}}#86efac{{end}}">
        {{printf "%.0f" .PassRate}}%
      </div>
    </div>
  </div>
  <div style="background:#0f1117;border-radius:6px;padding:10px;margin-bottom:8px">
    <div style="font-size:.7rem;color:#64748b;margin-bottom:6px">ВРЕМЯ ЗАГРУЗКИ</div>
    <div style="display:flex;justify-content:space-between;font-size:.8rem">
      <span style="color:#64748b">среднее <span style="color:#94a3b8;font-weight:600">{{fmtSecs .AvgSetup}}</span></span>
      <span style="color:#64748b">мин <span style="color:#86efac;font-weight:600">{{fmtSecs .MinSetup}}</span></span>
      <span style="color:#64748b">макс <span style="color:#f87171;font-weight:600">{{fmtSecs .MaxSetup}}</span></span>
    </div>
  </div>
  <div style="background:#0f1117;border-radius:6px;padding:10px;margin-bottom:8px">
    <div style="font-size:.7rem;color:#64748b;margin-bottom:6px">ВРЕМЯ ТЕСТОВ</div>
    <div style="display:flex;justify-content:space-between;font-size:.8rem">
      <span style="color:#64748b">среднее <span style="color:#94a3b8;font-weight:600">{{fmtSecs .AvgTest}}</span></span>
      <span style="color:#64748b">мин <span style="color:#86efac;font-weight:600">{{fmtSecs .MinTest}}</span></span>
      <span style="color:#64748b">макс <span style="color:#f87171;font-weight:600">{{fmtSecs .MaxTest}}</span></span>
    </div>
  </div>
  <div style="background:#0f1117;border-radius:6px;padding:10px">
    <div style="font-size:.7rem;color:#64748b;margin-bottom:6px">ВРЕМЯ ПЕРЕЗАГРУЗКИ</div>
    <div style="display:flex;justify-content:space-between;font-size:.8rem">
      <span style="color:#64748b">среднее <span style="color:#94a3b8;font-weight:600">{{fmtSecs .AvgBoot}}</span></span>
      <span style="color:#64748b">мин <span style="color:#86efac;font-weight:600">{{fmtSecs .MinBoot}}</span></span>
      <span style="color:#64748b">макс <span style="color:#f87171;font-weight:600">{{fmtSecs .MaxBoot}}</span></span>
    </div>
  </div>
  <button class="hist-btn" onclick="openHistory('{{.Serial}}','{{.Model}}')">📋 история</button>
</div>
{{end}}
</div>
</div>
</div>

<div id="epanel"{{if not .DeviceEvents}} style="display:none"{{end}}>
<div style="padding:0 24px 24px">
<h2 style="font-size:.85rem;color:#64748b;text-transform:uppercase;letter-spacing:.07em;margin-bottom:12px">Подключения устройств</h2>
<div style="overflow-x:auto">
<table>
<thead><tr>
  <th>Время</th><th>Устройство</th><th>Событие</th><th>USB путь</th><th>VID:PID</th><th>Длительность</th>
</tr></thead>
<tbody id="etbody">
{{range .DeviceEvents}}
<tr>
  <td class="mono">{{.TS.Format "2006-01-02 15:04:05"}}</td>
  <td>
    <div class="mono">{{.Serial}}</div>
    {{if .Model}}<div class="device">{{.Model}}</div>{{end}}
  </td>
  <td>
    {{if eq .Event "connected"}}
      <span class="badge pass">подключился</span>
    {{else}}
      <span class="badge fail">отключился</span>
    {{end}}
  </td>
  <td class="mono" style="color:#94a3b8">{{if .USBPath}}{{.USBPath}}{{else}}—{{end}}</td>
  <td class="mono" style="color:#64748b">{{if .VID}}{{.VID}}:{{.PID}}{{else}}—{{end}}</td>
  <td style="color:#64748b" id="edur-{{.ID}}">—</td>
</tr>
{{end}}
</tbody>
</table>
</div>
</div>
</div>

<p class="empty" id="emsg"{{if .Runs}} style="display:none"{{end}}>Результатов пока нет.</p>
<div id="twrap" style="overflow-x:auto{{if not .Runs}};display:none{{end}}">
<table>
<thead><tr>
  <th>Время</th><th>Устройство</th><th>Итог</th>
  <th>Прошло</th><th>Упало</th><th>Ожидает</th>
  <th>Подготовка</th><th>Тесты</th><th>Перезагрузка</th><th>Батарея</th><th>Логи</th>
</tr></thead>
<tbody id="tbody">
{{range .Runs}}
<tr>
  <td class="mono">{{.FinishedAt.Format "2006-01-02 15:04:05"}}</td>
  <td>
    <div class="mono">{{.Serial}}</div>
    {{if .Model}}<div class="device">{{.Model}}</div>{{end}}
  </td>
  <td>
    {{if not .Found}}<span class="badge na">Н/Д</span>
    {{else if gt .Failing 0}}<span class="badge fail">УПАЛ</span>
    {{else}}<span class="badge pass">ПРОШЁЛ</span>{{end}}
  </td>
  <td style="color:#86efac">{{if .Found}}{{.Passing}}{{else}}—{{end}}</td>
  <td style="color:{{if gt .Failing 0}}#f87171{{else}}#64748b{{end}}">{{if .Found}}{{.Failing}}{{else}}—{{end}}</td>
  <td style="color:#64748b">{{if .Found}}{{.Pending}}{{else}}—{{end}}</td>
  <td style="color:#94a3b8" title="Инициализация + установка APK">{{fmtSecs (sub .TotalSeconds .TestSeconds)}}</td>
  <td style="color:#94a3b8" title="Выполнение тестов">{{fmtSecs .TestSeconds}}</td>
  <td class="{{if .BootOK}}boot-ok{{else}}boot-fail{{end}}">{{bootTime .}}</td>
  <td>
    {{if lt .BatteryPct 0}}—
    {{else if lt .BatteryPct 30}}<span style="color:#f87171">{{.BatteryPct}}%</span>
    {{else if lt .BatteryPct 50}}<span style="color:#fbbf24">{{.BatteryPct}}%</span>
    {{else}}<span style="color:#86efac">{{.BatteryPct}}%</span>{{end}}
  </td>
  <td>
    {{if .HasLogs}}
    <button class="log-btn"    onclick="openLog({{.ID}},'test')">тест</button>
    <button class="log-btn ab" onclick="openLog({{.ID}},'appium')">appium</button>
    {{end}}{{if .HasScreenshot}}<button class="log-btn sc" onclick="openScr({{.ID}})">📷 скрин</button>{{end}}
    {{if not .HasLogs}}{{if not .HasScreenshot}}—{{end}}{{end}}
  </td>
</tr>
{{end}}
</tbody>
</table>
</div>

<script>
var _mid=null,_mtype=null;

function p2(n){return String(n).padStart(2,'0')}
function battFmt(p){if(p===undefined||p<0)return '—';var c=p<30?'#f87171':p<50?'#fbbf24':'#86efac';return '<span style="color:'+c+'">'+p+'%</span>'}
function fmtD(iso){var d=new Date(iso);return d.getFullYear()+'-'+p2(d.getMonth()+1)+'-'+p2(d.getDate())+' '+p2(d.getHours())+':'+p2(d.getMinutes())+':'+p2(d.getSeconds())}
function fmtS(s){if(!s||s<=0)return '—';s=Math.floor(s);return s<60?s+'с':Math.floor(s/60)+'м '+(s%60)+'с'}
function esc(s){return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')}

function renderTable(runs){
  var rc=document.getElementById('rcount'),
      em=document.getElementById('emsg'),
      tw=document.getElementById('twrap'),
      tb=document.getElementById('tbody');
  rc.textContent=(runs?runs.length:0)+' записей';
  if(!runs||!runs.length){tw.style.display='none';em.style.display='';return}
  em.style.display='none';tw.style.display='';
  tb.innerHTML=runs.map(function(r){
    var setup=Math.max(0,(r.total_seconds||0)-(r.test_seconds||0));
    var badge=!r.found?'<span class="badge na">Н/Д</span>':r.failing>0?'<span class="badge fail">УПАЛ</span>':'<span class="badge pass">ПРОШЁЛ</span>';
    var fc=r.failing>0?'#f87171':'#64748b';
    var bc=r.boot_ok?'boot-ok':'boot-fail';
    var boot=r.boot_ok?fmtS(r.boot_seconds):'—';
    var logs='';
    if(r.has_logs)logs+='<button class="log-btn" onclick="openLog('+r.id+',\'test\')">тест</button><button class="log-btn ab" onclick="openLog('+r.id+',\'appium\')">appium</button>';
    if(r.has_screenshot)logs+='<button class="log-btn sc" onclick="openScr('+r.id+')">📷 скрин</button>';
    if(!logs)logs='—';
    return '<tr>'+
      '<td class="mono">'+fmtD(r.finished_at)+'</td>'+
      '<td><div class="mono">'+esc(r.serial)+'</div>'+(r.model?'<div class="device">'+esc(r.model)+'</div>':'')+'</td>'+
      '<td>'+badge+'</td>'+
      '<td style="color:#86efac">'+(r.found?r.passing:'—')+'</td>'+
      '<td style="color:'+fc+'">'+(r.found?r.failing:'—')+'</td>'+
      '<td style="color:#64748b">'+(r.found?r.pending:'—')+'</td>'+
      '<td style="color:#94a3b8">'+fmtS(setup)+'</td>'+
      '<td style="color:#94a3b8">'+fmtS(r.test_seconds)+'</td>'+
      '<td class="'+bc+'">'+boot+'</td>'+
      '<td>'+battFmt(r.battery_pct)+'</td>'+
      '<td>'+logs+'</td>'+
    '</tr>';
  }).join('');
}

function renderStats(stats){
  var sp=document.getElementById('spanel'),
      sg=document.getElementById('sgrid');
  if(!stats||!stats.length){sp.style.display='none';return}
  sp.style.display='';
  sg.innerHTML=stats.map(function(st){
    var rate=st.total_tests?((st.total_tests-st.total_fail)/st.total_tests*100):0;
    var rc=rate<80?'#f87171':rate<100?'#fbbf24':'#86efac';
    var fc=st.total_fail>0?'#f87171':'#86efac';
    var badge=st.failed_runs===0?'<span class="badge pass">'+st.total_runs+' прогонов</span>':'<span class="badge fail">'+st.failed_runs+'/'+st.total_runs+' упало</span>';
    return '<div style="background:#1a1d27;border:1px solid #2d3148;border-radius:10px;padding:16px">'+
      '<div style="display:flex;justify-content:space-between;align-items:flex-start;margin-bottom:14px">'+
        '<div><div class="mono" style="font-size:.85rem;color:#e2e8f0">'+esc(st.serial)+'</div>'+(st.model?'<div style="font-size:.75rem;color:#64748b;margin-top:2px">'+esc(st.model)+'</div>':'')+'</div>'+
        '<div style="text-align:right">'+badge+'</div>'+
      '</div>'+
      '<div style="display:grid;grid-template-columns:1fr 1fr;gap:8px;margin-bottom:12px">'+
        '<div style="background:#0f1117;border-radius:6px;padding:10px"><div style="font-size:.7rem;color:#64748b;margin-bottom:4px">УПАЛО ТЕСТОВ</div><div style="font-size:1.1rem;font-weight:600;color:'+fc+'">'+st.total_fail+'<span style="font-size:.75rem;font-weight:400;color:#64748b"> / '+st.total_tests+'</span></div></div>'+
        '<div style="background:#0f1117;border-radius:6px;padding:10px"><div style="font-size:.7rem;color:#64748b;margin-bottom:4px">УСПЕШНОСТЬ</div><div style="font-size:1.1rem;font-weight:600;color:'+rc+'">'+rate.toFixed(0)+'%</div></div>'+
      '</div>'+
      '<div style="background:#0f1117;border-radius:6px;padding:10px;margin-bottom:8px">'+
        '<div style="font-size:.7rem;color:#64748b;margin-bottom:6px">ВРЕМЯ ЗАГРУЗКИ</div>'+
        '<div style="display:flex;justify-content:space-between;font-size:.8rem">'+
          '<span style="color:#64748b">среднее <span style="color:#94a3b8;font-weight:600">'+fmtS(st.avg_setup)+'</span></span>'+
          '<span style="color:#64748b">мин <span style="color:#86efac;font-weight:600">'+fmtS(st.min_setup)+'</span></span>'+
          '<span style="color:#64748b">макс <span style="color:#f87171;font-weight:600">'+fmtS(st.max_setup)+'</span></span>'+
        '</div>'+
      '</div>'+
      '<div style="background:#0f1117;border-radius:6px;padding:10px;margin-bottom:8px">'+
        '<div style="font-size:.7rem;color:#64748b;margin-bottom:6px">ВРЕМЯ ТЕСТОВ</div>'+
        '<div style="display:flex;justify-content:space-between;font-size:.8rem">'+
          '<span style="color:#64748b">среднее <span style="color:#94a3b8;font-weight:600">'+fmtS(st.avg_test)+'</span></span>'+
          '<span style="color:#64748b">мин <span style="color:#86efac;font-weight:600">'+fmtS(st.min_test)+'</span></span>'+
          '<span style="color:#64748b">макс <span style="color:#f87171;font-weight:600">'+fmtS(st.max_test)+'</span></span>'+
        '</div>'+
      '</div>'+
      '<div style="background:#0f1117;border-radius:6px;padding:10px">'+
        '<div style="font-size:.7rem;color:#64748b;margin-bottom:6px">ВРЕМЯ ПЕРЕЗАГРУЗКИ</div>'+
        '<div style="display:flex;justify-content:space-between;font-size:.8rem">'+
          '<span style="color:#64748b">среднее <span style="color:#94a3b8;font-weight:600">'+fmtS(st.avg_boot)+'</span></span>'+
          '<span style="color:#64748b">мин <span style="color:#86efac;font-weight:600">'+fmtS(st.min_boot)+'</span></span>'+
          '<span style="color:#64748b">макс <span style="color:#f87171;font-weight:600">'+fmtS(st.max_boot)+'</span></span>'+
        '</div>'+
      '</div>'+
      '<button class="hist-btn" onclick="openHistory(\''+esc(st.serial)+'\',\''+esc(st.model||'')+'\')">📋 история</button>'+
    '</div>';
  }).join('');
}

function renderEvents(events){
  var ep=document.getElementById('epanel'),
      tb=document.getElementById('etbody');
  if(!events||!events.length){ep.style.display='none';return}
  ep.style.display='';

  // Compute duration = time since previous event for the same serial (ascending order)
  var bySerial={};
  events.forEach(function(e){
    if(!bySerial[e.serial])bySerial[e.serial]=[];
    bySerial[e.serial].push(e);
  });
  // events are DESC, reverse per-device to get ascending for diff
  Object.values(bySerial).forEach(function(arr){arr.reverse()});
  var durMap={};
  Object.values(bySerial).forEach(function(arr){
    for(var i=1;i<arr.length;i++){
      durMap[arr[i].id]=Math.round((new Date(arr[i].ts)-new Date(arr[i-1].ts))/1000);
    }
  });

  tb.innerHTML=events.map(function(e){
    var evtBadge=e.event==='connected'?'<span class="badge pass">подключился</span>':'<span class="badge fail">отключился</span>';
    var usb=e.usb_path||'—';
    var vidpid=(e.vid&&e.pid)?e.vid+':'+e.pid:'—';
    var dur=durMap[e.id]!==undefined?fmtS(durMap[e.id]):'—';
    return '<tr>'+
      '<td class="mono">'+fmtD(e.ts)+'</td>'+
      '<td><div class="mono">'+esc(e.serial)+'</div>'+(e.model?'<div class="device">'+esc(e.model)+'</div>':'')+'</td>'+
      '<td>'+evtBadge+'</td>'+
      '<td class="mono" style="color:#94a3b8">'+esc(usb)+'</td>'+
      '<td class="mono" style="color:#64748b">'+esc(vidpid)+'</td>'+
      '<td style="color:#64748b">'+dur+'</td>'+
    '</tr>';
  }).join('');
}

async function refresh(){
  var p=new URLSearchParams(window.location.search);
  try{
    var rs=await fetch('/api/runs?'+p),
        ss=await fetch('/api/stats?'+p),
        es=await fetch('/api/device-events?'+p);
    if(rs.ok&&ss.ok&&es.ok){
      renderTable(await rs.json());
      renderStats(await ss.json());
      renderEvents(await es.json());
    }
  }catch(e){console.error('refresh:',e)}
}

// History modal
async function openHistory(serial, model){
  var modal=document.getElementById('modal'),
      title=document.getElementById('mtitle'),
      btnT=document.getElementById('btn-t'),
      btnA=document.getElementById('btn-a'),
      btnS=document.getElementById('btn-s'),
      pre=document.getElementById('mpre'),
      scr=document.getElementById('mscr'),
      hist=document.getElementById('mhist');
  title.textContent='История: '+serial+(model?' ('+model+')':'');
  btnT.style.display='none';btnA.style.display='none';btnS.style.display='none';
  pre.style.display='none';pre.textContent='';
  scr.style.display='none';scr.src='';
  hist.style.display='';
  hist.innerHTML='<p style="color:#64748b;padding:16px">Загрузка...</p>';
  modal.style.display='flex';
  try{
    var s=encodeURIComponent(serial);
    var[re,ee]=await Promise.all([
      fetch('/api/runs?serial='+s+'&limit=500'),
      fetch('/api/device-events?serial='+s+'&limit=500')
    ]);
    var runs=re.ok?await re.json():[];
    var evts=ee.ok?await ee.json():[];
    var items=[];
    (runs||[]).forEach(function(r){items.push({ts:r.finished_at,type:'run',data:r})});
    (evts||[]).forEach(function(e){items.push({ts:e.ts,type:'event',data:e})});
    items.sort(function(a,b){return new Date(b.ts)-new Date(a.ts)});
    if(!items.length){hist.innerHTML='<p style="color:#64748b;padding:16px">История пуста.</p>';return}
    var rows=items.map(function(item){
      if(item.type==='event'){
        var e=item.data;
        var badge=e.event==='connected'?'<span class="badge pass">подключился</span>':'<span class="badge fail">отключился</span>';
        var usb=e.usb_path?'<span class="mono" style="color:#94a3b8">'+esc(e.usb_path)+'</span>':'—';
        var vp=(e.vid&&e.pid)?'<span class="mono" style="color:#64748b"> '+esc(e.vid)+':'+esc(e.pid)+'</span>':'';
        return '<tr style="opacity:.8">'+
          '<td class="mono" style="font-size:.75rem">'+fmtD(e.ts)+'</td>'+
          '<td>'+badge+'</td>'+
          '<td style="color:#64748b">—</td>'+
          '<td style="color:#64748b">—</td>'+
          '<td style="color:#64748b">—</td>'+
          '<td>'+usb+vp+'</td>'+
          '<td style="color:#64748b">—</td>'+
        '</tr>';
      }else{
        var r=item.data;
        var badge=!r.found?'<span class="badge na">Н/Д</span>':r.failing>0?'<span class="badge fail">УПАЛ</span>':'<span class="badge pass">ПРОШЁЛ</span>';
        var setup=Math.max(0,(r.total_seconds||0)-(r.test_seconds||0));
        var fc=r.failing>0?'#f87171':'#86efac';
        var logs='';
        if(r.has_logs)logs+='<button class="log-btn" onclick="openLog('+r.id+',\'test\')">тест</button><button class="log-btn ab" onclick="openLog('+r.id+',\'appium\')">appium</button>';
        if(r.has_screenshot)logs+='<button class="log-btn sc" onclick="openScr('+r.id+')">📷</button>';
        if(!logs)logs='—';
        return '<tr>'+
          '<td class="mono" style="font-size:.75rem">'+fmtD(r.finished_at)+'</td>'+
          '<td>'+badge+'</td>'+
          '<td><span style="color:#86efac">'+(r.found?r.passing:'—')+'</span> / <span style="color:'+fc+'">'+(r.found?r.failing:'—')+'</span></td>'+
          '<td style="color:#94a3b8">'+fmtS(setup)+' / '+fmtS(r.test_seconds)+'</td>'+
          '<td>'+battFmt(r.battery_pct)+'</td>'+
          '<td style="color:#64748b">—</td>'+
          '<td>'+logs+'</td>'+
        '</tr>';
      }
    }).join('');
    hist.innerHTML='<table style="width:100%"><thead><tr>'+
      '<th>Время</th><th>Событие</th><th>Прошло / Упало</th><th>Подготовка / Тесты</th><th>Батарея</th><th>USB / VID:PID</th><th>Логи</th>'+
      '</tr></thead><tbody>'+rows+'</tbody></table>';
  }catch(e){hist.innerHTML='<p style="color:#f87171;padding:16px">Ошибка: '+e+'</p>'}
}

// SSE — auto-reconnects built into EventSource
(function(){
  var st=document.getElementById('cst');
  var es=new EventSource('/events');
  es.onopen=function(){st.textContent='● онлайн';st.style.color='#86efac'};
  es.onmessage=function(e){if(e.data==='refresh')refresh()};
  es.onerror=function(){st.textContent='○ переподключение...';st.style.color='#f87171'};
})();

// Modal
async function openLog(id,type){
  _mid=id;_mtype=type;
  document.getElementById('mtitle').textContent=(type==='test'?'Лог теста':'Лог Appium')+' #'+id;
  document.getElementById('btn-t').className='mtab'+(type==='test'?' on':'');document.getElementById('btn-t').style.display='';
  document.getElementById('btn-a').className='mtab'+(type==='appium'?' on':'');document.getElementById('btn-a').style.display='';
  document.getElementById('btn-s').style.display='none';
  var hist=document.getElementById('mhist');hist.style.display='none';hist.innerHTML='';
  var pre=document.getElementById('mpre'),scr=document.getElementById('mscr');
  pre.style.display='';
  pre.textContent='Загрузка...';
  // Show screenshot at top if it exists
  scr.style.display='block';
  scr.src='/logs/'+id+'/screen.png';
  document.getElementById('modal').style.display='flex';
  try{
    var r=await fetch('/api/log?id='+id+'&type='+type);
    pre.textContent=r.ok?await r.text():'Ошибка '+r.status+': лог недоступен';
  }catch(e){pre.textContent='Ошибка: '+e}
  var b=document.getElementById('mbody');b.scrollTop=b.scrollHeight;
}
// Open screenshot-only view
function openScr(id){
  _mid=id;_mtype='screenshot';
  document.getElementById('mtitle').textContent='Скриншот #'+id;
  document.getElementById('btn-t').className='mtab';document.getElementById('btn-t').style.display='';
  document.getElementById('btn-a').className='mtab';document.getElementById('btn-a').style.display='';
  document.getElementById('btn-s').className='mtab on';
  document.getElementById('btn-s').style.display='';
  var hist=document.getElementById('mhist');hist.style.display='none';hist.innerHTML='';
  var pre=document.getElementById('mpre'),scr=document.getElementById('mscr');
  pre.style.display='none';
  pre.textContent='';
  scr.style.display='block';
  scr.src='/logs/'+id+'/screen.png';
  scr.onerror=function(){pre.style.display='';pre.textContent='Скриншот недоступен';scr.style.display='none'};
  document.getElementById('modal').style.display='flex';
}
function switchLog(t){
  if(_mid===null)return;
  if(t==='screenshot')openScr(_mid);
  else openLog(_mid,t);
}
function closeModal(){var h=document.getElementById('mhist');document.getElementById('modal').style.display='none';document.getElementById('mscr').src='';h.style.display='none';h.innerHTML='';document.getElementById('btn-t').style.display='';document.getElementById('btn-a').style.display=''}
document.addEventListener('keydown',function(e){if(e.key==='Escape')closeModal()});
</script>
</body>
</html>`
