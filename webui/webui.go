package webui

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baragoon/ofelia/cli"
	"github.com/baragoon/ofelia/core"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/robfig/cron/v3"
)

const localHostKey = "local"
const defaultWebUIRefreshSeconds = 10

type webUIJob struct {
	Name             string
	Type             string
	Schedule         string
	Command          string
	MultilineCommand bool
	Target           string
	NextRun          time.Time
	LastRun          time.Time
	Running          bool
	LastOutput       string
	LastExitOK       *bool
}

var secretEqRe = regexp.MustCompile(
	`(?i)\b((?:api[_\-]?key|password|passwd|secret|token)\s*=\s*)([^\s,;&|"'\n]+)`,
)

var secretHdrRe = regexp.MustCompile(
	`(?im)\b((?:api[_\-]?key|authorization|x-auth-token|x-api-key|token)\s*:\s*)([^\n"']+)`,
)

func maskSecrets(s string) string {
	s = secretEqRe.ReplaceAllString(s, "${1}***")
	s = secretHdrRe.ReplaceAllString(s, "${1}***")
	return s
}

var httpStatusRe = regexp.MustCompile(`(?m)^HTTP/\S+\s+(\d{3})`)

func outputContainsHTTPFailure(output string) bool {
	for _, m := range httpStatusRe.FindAllStringSubmatch(output, -1) {
		if len(m) >= 2 && (m[1][0] != '2') {
			return true
		}
	}
	return false
}

type lastExecProvider interface {
	GetLastExecution() *core.Execution
}

// Returns lastRun, exitOK, output for a job's last execution.
func lastRunFields(p lastExecProvider) (lastRun time.Time, exitOK *bool, output string) {
	exec := p.GetLastExecution()
	if exec == nil {
		return
	}
	lastRun = exec.Date
	ok := !exec.Failed && !exec.Skipped
	exitOK = &ok
	// Prefer OutputStream if available, else fallback to fmt.Sprint(exec)
	if exec.OutputStream != nil {
		output = exec.OutputStream.String()
	} else {
		output = fmt.Sprint(exec)
	}
	return
}

type webUIHost struct {
	Key                  string
	Title                string
	Jobs                 []webUIJob
	JobCount             int
	RunningCount         int
	NextRun              time.Time
	HasMultilineCommands bool
}

type webUIState struct {
	Hosts        []webUIHost
	TotalHosts   int
	TotalJobs    int
	TotalRunning int
}

type webUIServer struct {
	server         *http.Server
	logger         core.Logger
	config         *cli.Config
	refreshSeconds int
}

// NewWebUIServer constructs a web UI server and registers HTTP routes.
// It satisfies the cli.UIServer interface.
func NewWebUIServer(bind string, refreshSeconds int, config *cli.Config, logger core.Logger) cli.UIServer {
	h := &webUIServer{
		logger:         logger,
		config:         config,
		refreshSeconds: normalizeRefreshSeconds(refreshSeconds),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleHosts)
	mux.HandleFunc("/hosts/", h.handleHostJobs)

	protected := RequireWebUIAuth(mux)

	h.server = &http.Server{
		Addr:              bind,
		Handler:           protected,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	return h
}

type webUIHostsPage struct {
	webUIState
	RefreshSeconds int
}

type webUIHostJobsPage struct {
	webUIHost
	RefreshSeconds int
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

// Template helpers and templates

var templateFuncMap = template.FuncMap{
	"notZero": func(t time.Time) bool { return !t.IsZero() },
	"fmtTime": func(t time.Time) string {
		if t.IsZero() {
			return "\u2014"
		}
		return t.Format("Jan 2, 15:04:05")
	},
	"relNext": func(t time.Time) string {
		if t.IsZero() {
			return "\u2014"
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
	"statusClass": func(exitOK *bool) string {
		if exitOK == nil {
			return ""
		}
		if *exitOK {
			return " last-ok"
		}
		return " last-fail"
	},
}

var hostsTemplate = template.Must(template.New("hosts").Funcs(templateFuncMap).Parse(`<!doctype html>
<html lang="en" data-theme="auto">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="{{.RefreshSeconds}}">
  <title>Ofelia</title>
  <style>
		:root,[data-theme="light"] { --bg:#f0f2f5;--fg:#1a1f36;--header-bg:#0d1117;--header-fg:#fff;--card-bg:#fff;--card-border:#e5e7eb;--ok:#3fb950;--fail:#f85149;--running:#1a7f37; }
		[data-theme="dark"] { --bg:#181a1b;--fg:#e5e7eb;--header-bg:#23272e;--header-fg:#fff;--card-bg:#23272e;--card-border:#333843;--ok:#58d26a;--fail:#ff7b72;--running:#4caf50; }
		@media (prefers-color-scheme: dark) {
			[data-theme="auto"] { --bg:#181a1b;--fg:#e5e7eb;--header-bg:#23272e;--header-fg:#fff;--card-bg:#23272e;--card-border:#333843;--ok:#58d26a;--fail:#ff7b72;--running:#4caf50; }
		}
    *,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
	body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:var(--bg);color:var(--fg);min-height:100vh;color-scheme:light dark}
    header{background:var(--header-bg);color:var(--header-fg);padding:.65rem 1.5rem;display:flex;align-items:center;gap:.6rem}
    .brand-dot{color:var(--ok);font-size:1.1rem}.brand{font-size:1rem;font-weight:700;letter-spacing:-.2px}
    .stats-bar{margin-left:auto;display:flex;gap:1.25rem;font-size:.8rem;color:var(--header-fg);align-items:center;opacity:.7}
    .stats-bar b{color:var(--header-fg)}.stat-running b{color:var(--ok)}
	main{max-width:1100px;margin:1.5rem auto;padding:0 1rem 3rem}
    .section-label{font-size:.72rem;font-weight:600;text-transform:uppercase;letter-spacing:.08em;color:#6b7280;margin-bottom:.75rem}
	.host-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(320px,1fr));gap:.8rem;justify-content:start}
	.host-card{background:var(--card-bg);border:1px solid var(--card-border);border-radius:8px;padding:1.2rem 1.3rem 1.25rem;text-decoration:none;color:inherit;transition:border-color .15s,box-shadow .15s;display:block;min-height:92px}
    .host-card:hover{border-color:var(--ok);box-shadow:0 2px 10px rgba(63,185,80,.12)}
		.card-name{font-size:.88rem;font-weight:600;margin-bottom:.3rem;position:relative;display:block;padding-left:.7rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
    .status-dot{display:inline-block;position:absolute;left:0;top:50%;transform:translateY(-50%);width:7px;height:7px;background:var(--ok);border-radius:50%}
    .card-info{font-size:.78rem;color:#6b7280}
		@media (max-width:720px){.host-grid{grid-template-columns:1fr}.host-card{min-height:auto}.card-name{white-space:normal;overflow:visible;text-overflow:clip}}
  </style>
</head>
<body>
  <header>
    <span class="brand-dot">&#11041;</span>
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
<html lang="en" data-theme="auto">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="{{.RefreshSeconds}}">
  <title>Ofelia &mdash; {{.Title}}</title>
  <style>
		:root,[data-theme="light"]{--bg:#f0f2f5;--fg:#1a1f36;--header-bg:#0d1117;--header-fg:#fff;--card-bg:#fff;--card-border:#e5e7eb;--ok:#3fb950;--fail:#f85149;--running:#1a7f37}
		[data-theme="dark"]{--bg:#181a1b;--fg:#e5e7eb;--header-bg:#23272e;--header-fg:#fff;--card-bg:#23272e;--card-border:#333843;--ok:#58d26a;--fail:#ff7b72;--running:#4caf50}
		@media (prefers-color-scheme: dark){
			[data-theme="auto"]{--bg:#181a1b;--fg:#e5e7eb;--header-bg:#23272e;--header-fg:#fff;--card-bg:#23272e;--card-border:#333843;--ok:#58d26a;--fail:#ff7b72;--running:#4caf50}
		}
    *,*::before,*::after{box-sizing:border-box;margin:0;padding:0}
	body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;background:var(--bg);color:var(--fg);min-height:100vh;color-scheme:light dark}
    header{background:var(--header-bg);color:var(--header-fg);padding:.65rem 1.5rem;display:flex;align-items:center;gap:.6rem}
    .brand-dot{color:var(--ok);font-size:1.1rem}.brand{font-size:1rem;font-weight:700;letter-spacing:-.2px}
    .stats-bar{margin-left:auto;display:flex;gap:1.25rem;font-size:.8rem;color:var(--header-fg);opacity:.7;align-items:center}
    .stats-bar b{color:var(--header-fg)}.stat-running b{color:var(--ok)}
    main{max-width:880px;margin:1.5rem auto;padding:0 1rem 3rem}
    .back{display:inline-flex;align-items:center;gap:.3rem;text-decoration:none;color:var(--ok);font-size:.82rem;margin-bottom:.9rem}
    .back:hover{color:var(--fail)}
    .host-title{font-size:1rem;font-weight:600;margin-bottom:.7rem;color:var(--fg)}
    .job-list{display:grid;grid-template-columns:auto 1fr auto;column-gap:.8rem;row-gap:.45rem}
    .job-row{grid-column:1/-1;background:var(--card-bg);border:1px solid var(--card-border);border-left:3px solid #9ca3af;border-radius:6px;padding:.75rem 1rem;display:grid;grid-template-columns:subgrid;align-items:center}
    .job-row.last-ok{border-left-color:var(--ok)}.job-row.last-fail{border-left-color:var(--fail)}.job-row.is-running{border-left-color:var(--running)}
    .job-schedule{font-family:monospace;font-size:.76rem;background:var(--bg);border:1px solid var(--card-border);border-radius:4px;padding:.22rem .55rem;white-space:nowrap;color:var(--fg);align-self:start;margin-top:.1rem}
    .job-body{min-width:0}.job-name{font-weight:600;font-size:.9rem;margin-bottom:.15rem;word-break:break-word}
    .job-target{font-size:.78rem;color:var(--fg);margin-bottom:.1rem}
    .job-meta{margin-top:.4rem;display:flex;gap:1.1rem;flex-wrap:wrap;font-size:.76rem;color:var(--fg)}
    .lbl{font-size:.65rem;font-weight:700;text-transform:uppercase;letter-spacing:.05em;margin-right:.3rem}
    .lbl-next{color:var(--ok)}
    .job-badges{display:flex;flex-direction:column;align-items:flex-end;gap:.3rem;align-self:start;padding-top:.1rem}
    .badge{display:inline-block;font-size:.68rem;font-weight:600;border-radius:99px;padding:.15rem .55rem;white-space:nowrap;background:var(--bg);color:var(--fg)}
    .badge-running{background:var(--ok);color:var(--header-bg)}
  </style>
</head>
<body>
  <header>
    <span class="brand-dot">&#11041;</span>
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
      {{range $idx, $job := .Jobs}}
      <div class="job-row{{if $job.Running}} is-running{{else}}{{statusClass $job.LastExitOK}}{{end}}">
        <code class="job-schedule">{{$job.Schedule}}</code>
        <div class="job-body">
          <div class="job-name">{{$job.Name}}</div>
          {{if $job.Target}}<div class="job-target">{{$job.Target}}</div>{{end}}
          <div class="job-meta">
            <span><span class="lbl lbl-next">Next Run</span>{{if notZero $job.NextRun}}{{relNext $job.NextRun}} &middot; {{fmtTime $job.NextRun}}{{else}}&mdash;{{end}}</span>
            <span><span class="lbl">Last Run</span>{{if notZero $job.LastRun}}{{fmtTime $job.LastRun}}{{else}}&mdash;{{end}}</span>
          </div>
        </div>
        <div class="job-badges">
          {{if $job.Running}}<span class="badge badge-running">RUNNING</span>{{end}}
          <span class="badge">{{$job.Type}}</span>
        </div>
      </div>
      {{end}}
    </div>
  </main>
</body>
</html>`))

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

// ...existing code...

func buildWebUIState(config *cli.Config) webUIState {
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

	// Use exported accessors instead of reflection/unsafe
	var mu *sync.RWMutex
	var sh interface{ CronJobs() []cron.Entry }
	var dockerHandler interface {
		GetInternalDockerClients() map[string]*docker.Client
	}
	if config != nil {
		mu = config.GetMutex()
		sh = config.GetScheduler()
		dockerHandler = config.GetDockerHandler()
	}
	if mu != nil {
		mu.RLock()
		defer mu.RUnlock()
	}

	type cronTiming struct{ next, prev time.Time }
	timings := map[int]cronTiming{}
	if sh != nil {
		for _, e := range sh.CronJobs() {
			timings[int(e.ID)] = cronTiming{next: e.Next, prev: e.Prev}
		}
	}

	if dockerHandler != nil {
		for hostKey := range dockerHandler.GetInternalDockerClients() {
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
			lr, exitOK, lout := lastRunFields(&job.ExecJob.BareJob)
			wjob := webUIJob{
				Name:             name,
				Type:             "exec",
				Schedule:         job.Schedule,
				Command:          maskSecrets(job.Command),
				MultilineCommand: isMultilineCommand(job.Command),
				Target:           job.Container,
				NextRun:          t.next,
				LastRun:          lr,
				Running:          job.Running() > 0,
				LastOutput:       lout,
			}
			if !lr.IsZero() && exitOK != nil {
				wjob.LastExitOK = exitOK
			}
			getHost(host).Jobs = append(getHost(host).Jobs, wjob)
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
			lr, exitOK, lout := lastRunFields(&job.RunJob.BareJob)
			wjob := webUIJob{
				Name:             name,
				Type:             "run",
				Schedule:         job.Schedule,
				Command:          maskSecrets(job.Command),
				MultilineCommand: isMultilineCommand(job.Command),
				Target:           target,
				NextRun:          t.next,
				LastRun:          lr,
				Running:          job.Running() > 0,
				LastOutput:       lout,
			}
			if !lr.IsZero() && exitOK != nil {
				wjob.LastExitOK = exitOK
			}
			getHost(host).Jobs = append(getHost(host).Jobs, wjob)
		}

		for jobName, job := range config.ServiceJobs {
			host, name := splitHostPrefixedJobName(jobName)
			if strings.TrimSpace(job.DockerHost) != "" {
				host = job.DockerHost
			}
			t := timings[job.GetCronJobID()]
			lr, exitOK, lout := lastRunFields(&job.RunServiceJob.BareJob)
			wjob := webUIJob{
				Name:             name,
				Type:             "service-run",
				Schedule:         job.Schedule,
				Command:          maskSecrets(job.Command),
				MultilineCommand: isMultilineCommand(job.Command),
				Target:           job.Image,
				NextRun:          t.next,
				LastRun:          lr,
				Running:          job.Running() > 0,
				LastOutput:       lout,
			}
			if !lr.IsZero() && exitOK != nil {
				wjob.LastExitOK = exitOK
			}
			getHost(host).Jobs = append(getHost(host).Jobs, wjob)
		}

		for jobName, job := range config.LocalJobs {
			t := timings[job.GetCronJobID()]
			lr, exitOK, lout := lastRunFields(&job.LocalJob.BareJob)
			wjob := webUIJob{
				Name:             jobName,
				Type:             "local",
				Schedule:         job.Schedule,
				Command:          maskSecrets(job.Command),
				MultilineCommand: isMultilineCommand(job.Command),
				Target:           job.Dir,
				NextRun:          t.next,
				LastRun:          lr,
				Running:          job.Running() > 0,
				LastOutput:       lout,
			}
			if !lr.IsZero() && exitOK != nil {
				wjob.LastExitOK = exitOK
			}
			getHost(localHostKey).Jobs = append(getHost(localHostKey).Jobs, wjob)
		}
	}

	hostList := make([]webUIHost, 0, len(hosts))
	totalJobs := 0
	totalRunning := 0

	for _, host := range hosts {
		// Deterministically sort jobs by Name, then Type
		if len(host.Jobs) > 1 {
			sort.Slice(host.Jobs, func(i, j int) bool {
				if host.Jobs[i].Name != host.Jobs[j].Name {
					return host.Jobs[i].Name < host.Jobs[j].Name
				}
				return host.Jobs[i].Type < host.Jobs[j].Type
			})
		}
		host.JobCount = len(host.Jobs)
		for _, j := range host.Jobs {
			if j.Running {
				host.RunningCount++
				totalRunning++
			}
			if j.MultilineCommand {
				host.HasMultilineCommands = true
			}
			if !j.NextRun.IsZero() && (host.NextRun.IsZero() || j.NextRun.Before(host.NextRun)) {
				host.NextRun = j.NextRun
			}
		}
		totalJobs += len(host.Jobs)
		hostList = append(hostList, *host)
	}

	// Deterministically sort hosts: localHostKey first, then by Key
	if len(hostList) > 1 {
		sort.Slice(hostList, func(i, j int) bool {
			if hostList[i].Key == localHostKey {
				return true
			}
			if hostList[j].Key == localHostKey {
				return false
			}
			return hostList[i].Key < hostList[j].Key
		})
	}

	return webUIState{
		Hosts:        hostList,
		TotalHosts:   len(hostList),
		TotalJobs:    totalJobs,
		TotalRunning: totalRunning,
	}
}

// Template helpers and templates (copied from cli/web_ui.go)
// ...existing code...

// ...existing code...

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

func isMultilineCommand(command string) bool {
	cmd := strings.TrimSpace(command)
	if strings.Contains(cmd, "\n") {
		return true
	}

	// Consider shell line-continuation style commands as multiline input.
	return strings.Contains(cmd, "\\ ")
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
