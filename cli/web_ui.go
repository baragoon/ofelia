package cli

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/baragoon/ofelia/core"
)

const localHostKey = "local"
const defaultWebUIRefreshSeconds = 10

type webUIJob struct {
	Name     string
	Type     string
	Schedule string
	Command  string
	Target   string
	NextRun  time.Time
	LastRun  time.Time
	Running  bool
}

type webUIHost struct {
	Key          string
	Title        string
	Jobs         []webUIJob
	JobCount     int
	RunningCount int
	NextRun      time.Time
}

type webUIState struct {
	Hosts        []webUIHost
	TotalHosts   int
	TotalJobs    int
	TotalRunning int
}

type webUIHostsPage struct {
	webUIState
	RefreshSeconds int
}

type webUIHostJobsPage struct {
	webUIHost
	RefreshSeconds int
}

type webUIServer struct {
	server         *http.Server
	logger         core.Logger
	config         *Config
	refreshSeconds int
}

func newWebUIServer(bind string, refreshSeconds int, config *Config, logger core.Logger) *webUIServer {
	h := &webUIServer{
		logger:         logger,
		config:         config,
		refreshSeconds: normalizeRefreshSeconds(refreshSeconds),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleHosts)
	mux.HandleFunc("/hosts/", h.handleHostJobs)

	h.server = &http.Server{
		Addr:              bind,
		Handler:           mux,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	return h
}

func (s *webUIServer) Start() error {
	listener, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		s.logger.Errorf("web UI server failed to bind on %s: %v", s.server.Addr, err)
		return err
	}

	s.logger.Noticef("Web UI listening on %s", s.server.Addr)
	go func() {
		if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Errorf("web UI server stopped with error: %v", err)
		}
	}()

	return nil
}

func (s *webUIServer) Stop(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

func (s *webUIServer) handleHosts(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	state := buildWebUIState(s.config)
	page := webUIHostsPage{webUIState: state, RefreshSeconds: s.refreshSeconds}
	if err := hostsTemplate.Execute(w, page); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *webUIServer) handleHostJobs(w http.ResponseWriter, r *http.Request) {
	hostKey := strings.TrimPrefix(r.URL.Path, "/hosts/")
	hostKey = strings.TrimSpace(strings.Trim(hostKey, "/"))
	if hostKey == "" {
		http.NotFound(w, r)
		return
	}

	state := buildWebUIState(s.config)
	for _, host := range state.Hosts {
		if host.Key != hostKey {
			continue
		}

		page := webUIHostJobsPage{webUIHost: host, RefreshSeconds: s.refreshSeconds}
		if err := hostJobsTemplate.Execute(w, page); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	http.NotFound(w, r)
}

func buildWebUIState(config *Config) webUIState {
	hosts := make(map[string]*webUIHost)
	getHost := func(key string) *webUIHost {
		key = strings.TrimSpace(key)
		if key == "" {
			key = localHostKey
		}

		h, ok := hosts[key]
		if ok {
			return h
		}

		h = &webUIHost{Key: key, Title: hostTitle(key)}
		hosts[key] = h
		return h
	}

	if config != nil {
		config.mu.RLock()
		defer config.mu.RUnlock()
	}

	// Build a timing map from the scheduler's live cron entries.
	type cronTiming struct{ next, prev time.Time }
	timings := map[int]cronTiming{}
	if config != nil && config.sh != nil {
		for _, e := range config.sh.CronJobs() {
			timings[int(e.ID)] = cronTiming{next: e.Next, prev: e.Prev}
		}
	}

	if config != nil && config.dockerHandler != nil {
		for hostKey := range config.dockerHandler.GetInternalDockerClients() {
			_ = getHost(hostKey)
		}
	}

	if config != nil {
		for jobName, job := range config.ExecJobs {
			host, name := splitHostPrefixedJobName(jobName)
			if strings.TrimSpace(job.DockerHost) != "" {
				host = job.DockerHost
			}
			name = trimContainerPrefixedJobName(name)
			t := timings[job.GetCronJobID()]
			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
				Name:     name,
				Type:     "exec",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   job.Container,
				NextRun:  t.next,
				LastRun:  t.prev,
				Running:  job.Running() > 0,
			})
		}

		for jobName, job := range config.RunJobs {
			host, name := splitHostPrefixedJobName(jobName)
			if strings.TrimSpace(job.DockerHost) != "" {
				host = job.DockerHost
			}
			target := strings.TrimSpace(job.Image)
			if target == "" {
				target = strings.TrimSpace(job.Container)
			}
			t := timings[job.GetCronJobID()]
			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
				Name:     name,
				Type:     "run",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   target,
				NextRun:  t.next,
				LastRun:  t.prev,
				Running:  job.Running() > 0,
			})
		}

		for jobName, job := range config.ServiceJobs {
			host, name := splitHostPrefixedJobName(jobName)
			if strings.TrimSpace(job.DockerHost) != "" {
				host = job.DockerHost
			}
			t := timings[job.GetCronJobID()]
			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
				Name:     name,
				Type:     "service-run",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   job.Image,
				NextRun:  t.next,
				LastRun:  t.prev,
				Running:  job.Running() > 0,
			})
		}

		for jobName, job := range config.LocalJobs {
			t := timings[job.GetCronJobID()]
			getHost(localHostKey).Jobs = append(getHost(localHostKey).Jobs, webUIJob{
				Name:     jobName,
				Type:     "local",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   job.Dir,
				NextRun:  t.next,
				LastRun:  t.prev,
				Running:  job.Running() > 0,
			})
		}
	}

	hostList := make([]webUIHost, 0, len(hosts))
	totalJobs := 0
	totalRunning := 0
	for _, host := range hosts {
		sort.Slice(host.Jobs, func(i, j int) bool {
			if host.Jobs[i].Name != host.Jobs[j].Name {
				return host.Jobs[i].Name < host.Jobs[j].Name
			}
			return host.Jobs[i].Type < host.Jobs[j].Type
		})
		host.JobCount = len(host.Jobs)
		for _, j := range host.Jobs {
			if j.Running {
				host.RunningCount++
				totalRunning++
			}
			if !j.NextRun.IsZero() && (host.NextRun.IsZero() || j.NextRun.Before(host.NextRun)) {
				host.NextRun = j.NextRun
			}
		}
		totalJobs += len(host.Jobs)
		hostList = append(hostList, *host)
	}

	sort.Slice(hostList, func(i, j int) bool {
		if hostList[i].Key == localHostKey {
			return true
		}
		if hostList[j].Key == localHostKey {
			return false
		}
		return hostList[i].Key < hostList[j].Key
	})

	return webUIState{
		Hosts:        hostList,
		TotalHosts:   len(hostList),
		TotalJobs:    totalJobs,
		TotalRunning: totalRunning,
	}
}

func splitHostPrefixedJobName(name string) (host, jobName string) {
	host = localHostKey
	jobName = strings.TrimSpace(name)

	parts := strings.SplitN(name, "::", 2)
	if len(parts) != 2 {
		return host, jobName
	}

	host = strings.TrimSpace(parts[0])
	jobName = strings.TrimSpace(parts[1])
	if host == "" {
		host = localHostKey
	}
	if jobName == "" {
		jobName = strings.TrimSpace(name)
	}

	return host, jobName
}

func trimContainerPrefixedJobName(name string) string {
	trimmed := strings.TrimSpace(name)
	parts := strings.SplitN(trimmed, "::", 2)
	if len(parts) != 2 {
		return trimmed
	}

	jobName := strings.TrimSpace(parts[1])
	if jobName == "" {
		return trimmed
	}

	return jobName
}

func hostTitle(key string) string {
	if key == localHostKey {
		return "Local Host"
	}

	if idx := strings.LastIndex(key, "_"); idx > 0 && idx < len(key)-1 {
		port := key[idx+1:]
		allDigits := true
		for _, ch := range port {
			if ch < '0' || ch > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return fmt.Sprintf("%s:%s", key[:idx], port)
		}
	}

	return key
}

func normalizeRefreshSeconds(refreshSeconds int) int {
	if refreshSeconds <= 0 {
		return defaultWebUIRefreshSeconds
	}

	return refreshSeconds
}

var templateFuncMap = template.FuncMap{
	"notZero": func(t time.Time) bool { return !t.IsZero() },
	"fmtTime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("Jan 2, 15:04:05")
	},
	"relNext": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		d := time.Until(t)
		if d <= 0 {
			return "overdue"
		}
		if d < time.Minute {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
		if d < time.Hour {
			return fmt.Sprintf("%dm", int(d.Minutes()))
		}
		h := int(d.Hours())
		m := int(d.Minutes()) % 60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh %dm", h, m)
	},
}

var hostsTemplate = template.Must(template.New("hosts").Funcs(templateFuncMap).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="{{.RefreshSeconds}}">
  <title>Ofelia</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f0f2f5; color: #1a1f36; min-height: 100vh; }
    header { background: #0d1117; color: #fff; padding: .65rem 1.5rem; display: flex; align-items: center; gap: .6rem; }
    .brand-dot { color: #3fb950; font-size: 1.1rem; }
    .brand { font-size: 1rem; font-weight: 700; letter-spacing: -.2px; }
    .stats-bar { margin-left: auto; display: flex; gap: 1.25rem; font-size: .8rem; color: rgba(255,255,255,.5); align-items: center; }
    .stats-bar b { color: #fff; }
    .stat-running b { color: #3fb950; }
    main { max-width: 880px; margin: 1.5rem auto; padding: 0 1rem 3rem; }
    .section-label { font-size: .72rem; font-weight: 600; text-transform: uppercase; letter-spacing: .08em; color: #6b7280; margin-bottom: .75rem; }
	.host-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(230px, 1fr)); gap: .8rem; }
	.host-card { background: #fff; border: 1px solid #e5e7eb; border-radius: 8px; padding: 1.2rem 1.3rem 1.25rem; min-height: 110px; text-decoration: none; color: inherit; transition: border-color .15s, box-shadow .15s; display: block; }
    .host-card:hover { border-color: #3fb950; box-shadow: 0 2px 10px rgba(63,185,80,.12); }
    .card-name { font-size: .88rem; font-weight: 600; margin-bottom: .3rem; word-break: break-all; display: flex; align-items: center; gap: .35rem; }
    .status-dot { display: inline-block; width: 7px; height: 7px; background: #3fb950; border-radius: 50%; flex-shrink: 0; }
    .card-info { font-size: .78rem; color: #6b7280; }
    footer { text-align: center; font-size: .75rem; color: #9ca3af; margin-top: 2.5rem; }
  </style>
</head>
<body>
  <header>
    <span class="brand-dot">⬡</span>
    <span class="brand">Ofelia</span>
    <div class="stats-bar">
      <div><b>{{.TotalJobs}}</b> total jobs</div>
      <div><b>{{.TotalHosts}}</b> hosts</div>
      {{if .TotalRunning}}<div class="stat-running"><b>{{.TotalRunning}}</b> running</div>{{end}}
    </div>
  </header>
  <main>
    <p class="section-label">Docker Hosts</p>
    <div class="host-grid">
      {{range .Hosts}}
      <a class="host-card" href="/hosts/{{.Key}}">
        <div class="card-name"><span class="status-dot"></span>{{.Title}}</div>
        <div class="card-info">{{.JobCount}} job{{if ne .JobCount 1}}s{{end}}{{if .RunningCount}} &middot; <b>{{.RunningCount}} running</b>{{end}}</div>
      </a>
      {{end}}
    </div>
  </main>
</body>
</html>`))

var hostJobsTemplate = template.Must(template.New("host-jobs").Funcs(templateFuncMap).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="{{.RefreshSeconds}}">
  <title>Ofelia &mdash; {{.Title}}</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: #f0f2f5; color: #1a1f36; min-height: 100vh; }
    header { background: #0d1117; color: #fff; padding: .65rem 1.5rem; display: flex; align-items: center; gap: .6rem; }
    .brand-dot { color: #3fb950; font-size: 1.1rem; }
    .brand { font-size: 1rem; font-weight: 700; letter-spacing: -.2px; }
    .stats-bar { margin-left: auto; display: flex; gap: 1.25rem; font-size: .8rem; color: rgba(255,255,255,.5); align-items: center; }
    .stats-bar b { color: #fff; }
    .stat-running b { color: #3fb950; }
    main { max-width: 880px; margin: 1.5rem auto; padding: 0 1rem 3rem; }
    .back { display: inline-flex; align-items: center; gap: .3rem; text-decoration: none; color: #6b7280; font-size: .82rem; margin-bottom: .9rem; }
    .back:hover { color: #3fb950; }
    .host-title { font-size: 1rem; font-weight: 600; margin-bottom: 1rem; color: #1a1f36; }
    .job-list { display: flex; flex-direction: column; gap: .45rem; }
    .job-row { background: #fff; border: 1px solid #e5e7eb; border-left: 3px solid #3fb950; border-radius: 6px; padding: .75rem 1rem; display: grid; grid-template-columns: auto 1fr auto; gap: .8rem; align-items: start; }
    .job-row.is-running { border-left-color: #1a7f37; }
    .job-schedule { font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: .76rem; background: #f0f4f8; border: 1px solid #dee2e6; border-radius: 4px; padding: .22rem .55rem; white-space: nowrap; color: #3d4f60; align-self: start; margin-top: .1rem; }
    .job-body { min-width: 0; }
    .job-name { font-weight: 600; font-size: .9rem; margin-bottom: .15rem; word-break: break-word; }
    .job-target { font-size: .78rem; color: #6b7280; margin-bottom: .1rem; }
    .job-cmd { font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: .75rem; color: #8b949e; overflow: hidden; white-space: nowrap; text-overflow: ellipsis; }
    .job-meta { margin-top: .4rem; display: flex; gap: 1.1rem; flex-wrap: wrap; font-size: .76rem; color: #6b7280; }
    .lbl { font-size: .65rem; font-weight: 700; text-transform: uppercase; letter-spacing: .05em; margin-right: .3rem; }
    .lbl-next { color: #3fb950; }
    .job-badges { display: flex; flex-direction: column; align-items: flex-end; gap: .3rem; align-self: start; padding-top: .1rem; }
    .badge { display: inline-block; font-size: .68rem; font-weight: 600; border-radius: 99px; padding: .15rem .55rem; white-space: nowrap; }
    .badge-type { background: #f3f4f6; color: #4b5563; }
    .badge-running { background: #dcfce7; color: #166534; }
    footer { text-align: center; font-size: .75rem; color: #9ca3af; margin-top: 2.5rem; }
  </style>
</head>
<body>
  <header>
    <span class="brand-dot">⬡</span>
    <span class="brand">Ofelia</span>
    <div class="stats-bar">
      <div><b>{{.JobCount}}</b> job{{if ne .JobCount 1}}s{{end}}</div>
      {{if .RunningCount}}<div class="stat-running"><b>{{.RunningCount}}</b> running</div>{{end}}
      {{if notZero .NextRun}}<div>next: <b>{{fmtTime .NextRun}}</b></div>{{end}}
    </div>
  </header>
  <main>
    <a class="back" href="/">&#8592; All hosts</a>
    <div class="host-title">{{.Title}}</div>
    <div class="job-list">
      {{range .Jobs}}
      <div class="job-row{{if .Running}} is-running{{end}}">
        <code class="job-schedule">{{.Schedule}}</code>
        <div class="job-body">
          <div class="job-name">{{.Name}}</div>
          {{if .Target}}<div class="job-target">{{.Target}}</div>{{end}}
          {{if .Command}}<div class="job-cmd">{{.Command}}</div>{{end}}
          <div class="job-meta">
            <span><span class="lbl lbl-next">Next Run</span>{{if notZero .NextRun}}{{relNext .NextRun}} &middot; {{fmtTime .NextRun}}{{else}}&mdash;{{end}}</span>
            <span><span class="lbl">Last Run</span>{{if notZero .LastRun}}{{fmtTime .LastRun}}{{else}}&mdash;{{end}}</span>
          </div>
        </div>
        <div class="job-badges">
          {{if .Running}}<span class="badge badge-running">RUNNING</span>{{end}}
          <span class="badge badge-type">{{.Type}}</span>
        </div>
      </div>
      {{end}}
    </div>
  </main>
</body>
</html>`))
