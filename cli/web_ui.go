package cli

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
	"time"

	"github.com/baragoon/ofelia/core"
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
	// LastOutput is the masked combined stdout+stderr of the most recent execution.
	// Empty when the job has never run.
	LastOutput string
	// LastExitOK is nil when the job has never run, true when it succeeded, false when it failed.
	LastExitOK *bool
}

// secretEqRe masks assignment-style secrets: key=value (env vars, URL params).
// Value is matched up to the next whitespace or common delimiter.
var secretEqRe = regexp.MustCompile(
	`(?i)\b((?:api[_\-]?key|password|passwd|secret|token)\s*=\s*)([^\s,;&|"'\n]+)`,
)

// secretHdrRe masks HTTP-header-style secrets: "Key: value" (rest of line is the value).
var secretHdrRe = regexp.MustCompile(
	`(?im)\b((?:api[_\-]?key|authorization|x-auth-token|x-api-key|token)\s*:\s*)([^\n"']+)`,
)

// maskSecrets replaces the value portion of recognized secret patterns with ***.
func maskSecrets(s string) string {
	s = secretEqRe.ReplaceAllString(s, "${1}***")
	s = secretHdrRe.ReplaceAllString(s, "${1}***")
	return s
}

// httpStatusRe matches the status code in an HTTP response line, e.g. "HTTP/1.1 404 Not Found".
var httpStatusRe = regexp.MustCompile(`(?m)^HTTP/\S+\s+(\d{3})`)

// outputContainsHTTPFailure returns true if the output contains at least one
// HTTP response line whose status code is NOT in the 2xx range.
func outputContainsHTTPFailure(output string) bool {
	for _, m := range httpStatusRe.FindAllStringSubmatch(output, -1) {
		if len(m) >= 2 && (m[1][0] != '2') {
			return true
		}
	}
	return false
}

// lastExecProvider is satisfied by every concrete job type (they all embed BareJob).
type lastExecProvider interface {
	GetLastExecution() *core.Execution
}

// lastRunFields extracts the last-run time, success flag, and masked output for any job.
// exitOK is nil when the job has never run.
func lastRunFields(p lastExecProvider) (lastRun time.Time, exitOK *bool, output string) {
	exec := p.GetLastExecution()
	if exec == nil {
		return
	}
	lastRun = exec.Date
	ok := !exec.Failed && !exec.Skipped
	if ok {
		combined := exec.OutputStream.String() + exec.ErrorStream.String()
		if outputContainsHTTPFailure(combined) {
			ok = false
		}
	}
	exitOK = &ok
	raw := exec.OutputStream.String()
	if errOut := exec.ErrorStream.String(); errOut != "" {
		if raw != "" {
			raw += "\n--- stderr ---\n"
		}
		raw += errOut
	}
	output = maskSecrets(raw)
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

	// Wrap with password protection middleware
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
			lr, exitOK, lout := lastRunFields(&job.ExecJob.BareJob)
			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
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
				LastExitOK:       exitOK,
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
			lr, exitOK, lout := lastRunFields(&job.RunJob.BareJob)
			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
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
				LastExitOK:       exitOK,
			})
		}

		for jobName, job := range config.ServiceJobs {
			host, name := splitHostPrefixedJobName(jobName)
			if strings.TrimSpace(job.DockerHost) != "" {
				host = job.DockerHost
			}
			t := timings[job.GetCronJobID()]
			lr, exitOK, lout := lastRunFields(&job.RunServiceJob.BareJob)
			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
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
				LastExitOK:       exitOK,
			})
		}

		for jobName, job := range config.LocalJobs {
			t := timings[job.GetCronJobID()]
			lr, exitOK, lout := lastRunFields(&job.LocalJob.BareJob)
			getHost(localHostKey).Jobs = append(getHost(localHostKey).Jobs, webUIJob{
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
				LastExitOK:       exitOK,
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
	// statusClass returns the CSS modifier class for a job row based on last run result.
	// nil = never run (grey), true = success (green), false = failed (red).
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
	<link rel="icon" type="image/png" href="/static/ofelia-ng.png">
  <title>Ofelia</title>
	<style>
		:root {
			--bg: #f0f2f5;
			--fg: #1a1f36;
			--header-bg: #0d1117;
			--header-fg: #fff;
			--card-bg: #fff;
			--card-border: #e5e7eb;
			--ok: #3fb950;
			--fail: #f85149;
			--running: #1a7f37;
		}
		[data-theme="dark"] {
			--bg: #181a1b;
			--fg: #e5e7eb;
			--header-bg: #23272e;
			--header-fg: #fff;
			--card-bg: #23272e;
			--card-border: #333843;
		}
		*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
		body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: var(--bg); color: var(--fg); min-height: 100vh; }
		header { background: var(--header-bg); color: var(--header-fg); padding: .65rem 1.5rem; display: flex; align-items: center; gap: .6rem; }
		.brand-dot { color: var(--ok); font-size: 1.1rem; }
		.brand { font-size: 1rem; font-weight: 700; letter-spacing: -.2px; }
		.stats-bar { margin-left: auto; display: flex; gap: 1.25rem; font-size: .8rem; color: var(--header-fg); align-items: center; opacity: .7; }
		.stats-bar b { color: var(--header-fg); }
		.stat-running b { color: var(--ok); }
		main { max-width: 880px; margin: 1.5rem auto; padding: 0 1rem 3rem; }
		.section-label { font-size: .72rem; font-weight: 600; text-transform: uppercase; letter-spacing: .08em; color: #6b7280; margin-bottom: .75rem; }
	.host-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(230px, 1fr)); gap: .8rem; }
	.host-card { background: var(--card-bg); border: 1px solid var(--card-border); border-radius: 8px; padding: 1.2rem 1.3rem 1.25rem; min-height: 110px; text-decoration: none; color: inherit; transition: border-color .15s, box-shadow .15s; display: block; }
		.host-card:hover { border-color: var(--ok); box-shadow: 0 2px 10px rgba(63,185,80,.12); }
		.card-name { font-size: .88rem; font-weight: 600; margin-bottom: .3rem; word-break: break-all; display: flex; align-items: center; gap: .35rem; }
		.status-dot { display: inline-block; width: 7px; height: 7px; background: var(--ok); border-radius: 50%; flex-shrink: 0; }
		.card-info { font-size: .78rem; color: #6b7280; }
		footer { text-align: center; font-size: .75rem; color: #9ca3af; margin-top: 2.5rem; }
		#theme-toggle {
			margin-left: 1.5rem;
			border-radius: 999px;
			border: 1px solid var(--card-border);
			background: var(--card-bg);
			color: var(--fg);
			font-size: .8rem;
			padding: .2rem .8rem;
			cursor: pointer;
		}
	</style>
</head>
<body>
	<header>
		<span class="brand-dot">⬡</span>
		<span class="brand">Ofelia</span>
		<button id="theme-toggle">🌗</button>
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
	<script>
	(function() {
		// Theme logic
		const root = document.documentElement;
		const themeBtn = document.getElementById('theme-toggle');
		function setTheme(theme, persist = true) {
			if (theme === 'auto') {
				if (persist) localStorage.setItem('ofelia-theme', 'auto');
				const sysTheme = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
				root.setAttribute('data-theme', sysTheme);
				themeBtn.textContent = '🌗';
			} else {
				root.setAttribute('data-theme', theme);
				if (persist) localStorage.setItem('ofelia-theme', theme);
				themeBtn.textContent = theme === 'dark' ? '🌞' : '🌙';
			}
		}
		function applyTheme() {
			const saved = localStorage.getItem('ofelia-theme') || 'auto';
			if (saved === 'auto') setTheme('auto', false);
			else setTheme(saved, true);
		}
		themeBtn.addEventListener('click', function() {
			// Three-state cycle: auto -> light -> dark -> auto
			const saved = localStorage.getItem('ofelia-theme') || 'auto';
			let next;
			if (saved === 'auto') next = 'light';
			else if (saved === 'light') next = 'dark';
			else next = 'auto';
			setTheme(next, true);
		});
		window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function() {
			if ((localStorage.getItem('ofelia-theme') || 'auto') === 'auto') applyTheme();
		});
		applyTheme();
	})();
	</script>
</body>
</html>`))

var hostJobsTemplate = template.Must(template.New("host-jobs").Funcs(templateFuncMap).Parse(`<!doctype html>
<html lang="en" data-theme="auto">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="{{.RefreshSeconds}}">
	<link rel="icon" type="image/gif" href="https://camo.githubusercontent.com/7f2c551d2a3c6af164a47b2d87599cd085404668b3249fecb774b33dbfd4504b/68747470733a2f2f776569726473706163652e646b2f4672616e636973636f4962616e657a2f47726170686963732f4f66656c69612e676966">
  <title>Ofelia &mdash; {{.Title}}</title>
	<style>
		:root {
			--bg: #f0f2f5;
			--fg: #1a1f36;
			--header-bg: #0d1117;
			--header-fg: #fff;
			--card-bg: #fff;
			--card-border: #e5e7eb;
			--ok: #3fb950;
			--fail: #f85149;
			--running: #1a7f37;
		}
		[data-theme="dark"] {
			--bg: #181a1b;
			--fg: #e5e7eb;
			--header-bg: #23272e;
			--header-fg: #fff;
			--card-bg: #23272e;
			--card-border: #333843;
		}
		   /* Only use variable-based rules here; remove duplicates below */
		   .host-card, .job-row { background: var(--card-bg); border-color: var(--card-border); }
		   .job-row.last-ok { border-left-color: var(--ok); }
		   .job-row.last-fail { border-left-color: var(--fail); }
		   .job-row.is-running { border-left-color: var(--running); }
	*, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
	body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; background: var(--bg); color: var(--fg); min-height: 100vh; }
	header { background: var(--header-bg); color: var(--header-fg); padding: .65rem 1.5rem; display: flex; align-items: center; gap: .6rem; }
	.brand-dot { color: var(--ok); font-size: 1.1rem; }
	.brand { font-size: 1rem; font-weight: 700; letter-spacing: -.2px; }
	.stats-bar { margin-left: auto; display: flex; gap: 1.25rem; font-size: .8rem; color: var(--header-fg); opacity: .7; align-items: center; }
	.stats-bar b { color: var(--header-fg); }
	.stat-running b { color: var(--ok); }
	main { max-width: 880px; margin: 1.5rem auto; padding: 0 1rem 3rem; }
	.back { display: inline-flex; align-items: center; gap: .3rem; text-decoration: none; color: var(--ok); font-size: .82rem; margin-bottom: .9rem; }
	.back:hover { color: var(--fail); }
	.host-title { font-size: 1rem; font-weight: 600; margin-bottom: .7rem; color: var(--fg); }
	.host-actions { margin-bottom: .9rem; display: flex; justify-content: flex-end; }
	.expand-all-btn { border: 1px solid var(--ok); border-radius: 999px; background: var(--card-bg); color: var(--ok); font-size: .74rem; font-weight: 700; padding: .32rem .72rem; cursor: pointer; letter-spacing: .01em; }
	.expand-all-btn:hover { border-color: var(--fail); background: var(--card-bg); }
	.expand-all-btn:focus-visible { outline: 2px solid var(--ok); outline-offset: 2px; }
	.job-list { display: flex; flex-direction: column; gap: .45rem; }
	.job-row { background: var(--card-bg); border: 1px solid var(--card-border); border-left: 3px solid #9ca3af; border-radius: 6px; padding: .75rem 1rem; display: grid; grid-template-columns: auto 1fr auto; gap: .8rem; align-items: start; }
	.job-row.last-ok { border-left-color: var(--ok); }
	.job-row.last-fail { border-left-color: var(--fail); }
	.job-row.is-running { border-left-color: var(--running); }
	.job-schedule { font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: .76rem; background: var(--bg); border: 1px solid var(--card-border); border-radius: 4px; padding: .22rem .55rem; white-space: nowrap; color: var(--fg); align-self: start; margin-top: .1rem; }
	.job-body { min-width: 0; }
	.job-name { font-weight: 600; font-size: .9rem; margin-bottom: .15rem; word-break: break-word; color: var(--fg); }
	.job-target { font-size: .78rem; color: var(--fg); margin-bottom: .1rem; }
	.job-cmd-wrap { margin-top: .12rem; }
	.job-cmd { font-family: "SFMono-Regular", Consolas, "Liberation Mono", Menlo, monospace; font-size: .75rem; color: var(--fg); overflow: hidden; display: -webkit-box; -webkit-line-clamp: 2; -webkit-box-orient: vertical; white-space: pre-wrap; word-break: break-word; background: var(--card-bg); }
	.job-cmd.expanded { display: block; -webkit-line-clamp: unset; }
	.cmd-expand-btn { margin-top: .25rem; border: 1px solid var(--ok); border-radius: 999px; background: var(--card-bg); color: var(--ok); font-size: .7rem; font-weight: 700; padding: .14rem .5rem; cursor: pointer; }
	.cmd-expand-btn:hover { background: var(--bg); border-color: var(--fail); }
	.cmd-expand-btn:focus-visible { outline: 2px solid var(--ok); outline-offset: 2px; }
	.job-meta { margin-top: .4rem; display: flex; gap: 1.1rem; flex-wrap: wrap; font-size: .76rem; color: var(--fg); }
	.lbl { font-size: .65rem; font-weight: 700; text-transform: uppercase; letter-spacing: .05em; margin-right: .3rem; }
	.lbl-next { color: var(--ok); }
	.last-run-info { cursor: default; border-bottom: 1px dashed var(--ok); }
	.last-run-info[title] { cursor: help; border-bottom-style: dotted; }
	.job-badges { display: flex; flex-direction: column; align-items: flex-end; gap: .3rem; align-self: start; padding-top: .1rem; }
	.badge { display: inline-block; font-size: .68rem; font-weight: 600; border-radius: 99px; padding: .15rem .55rem; white-space: nowrap; background: var(--bg); color: var(--fg); }
	.badge-type { background: var(--card-bg); color: var(--fg); }
	.badge-running { background: var(--ok); color: var(--header-bg); }
	footer { text-align: center; font-size: .75rem; color: var(--fg); margin-top: 2.5rem; }
		   #theme-toggle {
			   margin-left: 1.5rem;
			   border-radius: 999px;
			   border: 1px solid var(--card-border);
			   background: var(--card-bg);
			   color: var(--fg);
			   font-size: .8rem;
			   padding: .2rem .8rem;
			   cursor: pointer;
		   }
	   </style>
</head>
<body>
  <header>
    <span class="brand-dot">⬡</span>
    <span class="brand">Ofelia</span>
	<button id="theme-toggle">🌗</button>
    <div class="stats-bar">
      <div><b>{{.JobCount}}</b> job{{if ne .JobCount 1}}s{{end}}</div>
      {{if .RunningCount}}<div class="stat-running"><b>{{.RunningCount}}</b> running</div>{{end}}
      {{if notZero .NextRun}}<div>next: <b>{{fmtTime .NextRun}}</b></div>{{end}}
    </div>
  </header>
  <main>
    <a class="back" href="/">&#8592; All hosts</a>
    <div class="host-title">{{.Title}}</div>
		{{if .HasMultilineCommands}}
		<div class="host-actions">
			<button id="expand-all-btn" class="expand-all-btn" type="button">Expand all commands</button>
		</div>
		{{end}}
    <div class="job-list">
			{{range $idx, $job := .Jobs}}
			<div class="job-row{{if $job.Running}} is-running{{else}}{{statusClass $job.LastExitOK}}{{end}}">
				<code class="job-schedule">{{$job.Schedule}}</code>
        <div class="job-body">
					<div class="job-name">{{$job.Name}}</div>
					{{if $job.Target}}<div class="job-target">{{$job.Target}}</div>{{end}}
						{{if $job.Command}}
						<div class="job-cmd-wrap">
							<div id="cmd-{{$idx}}" class="job-cmd">{{$job.Command}}</div>
							<button type="button" class="cmd-expand-btn" data-target="cmd-{{$idx}}" data-expanded="false" aria-expanded="false">Expand command</button>
						</div>
						{{end}}
          <div class="job-meta">
						<span><span class="lbl lbl-next">Next Run</span>{{if notZero $job.NextRun}}{{relNext $job.NextRun}} &middot; {{fmtTime $job.NextRun}}{{else}}&mdash;{{end}}</span>
						<span><span class="lbl">Last Run</span>{{if notZero $job.LastRun}}<span class="last-run-info"{{if $job.LastOutput}} title="{{$job.LastOutput}}"{{end}}>{{fmtTime $job.LastRun}}</span>{{else}}&mdash;{{end}}</span>
          </div>
        </div>
        <div class="job-badges">
					{{if $job.Running}}<span class="badge badge-running">RUNNING</span>{{end}}
					<span class="badge badge-type">{{$job.Type}}</span>
        </div>
      </div>
      {{end}}
    </div>
  </main>
	<script>
	(function() {
	  // Theme logic
	  const root = document.documentElement;
	  const themeBtn = document.getElementById('theme-toggle');
	   function setTheme(theme, persist = true) {
		   if (theme === 'auto') {
			   // Store 'auto', but apply the system theme
			   if (persist) localStorage.setItem('ofelia-theme', 'auto');
			   const sysTheme = getSystemTheme();
			   root.setAttribute('data-theme', sysTheme);
			   themeBtn.textContent = '🌗';
		   } else {
			   root.setAttribute('data-theme', theme);
			   if (persist) localStorage.setItem('ofelia-theme', theme);
			   themeBtn.textContent = theme === 'dark' ? '🌞' : '🌙';
		   }
	   }
	  function getSystemTheme() {
	    return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
	  }
	   function applyTheme() {
		   const saved = localStorage.getItem('ofelia-theme') || 'auto';
		   if (saved === 'auto') setTheme('auto', false);
		   else setTheme(saved, true);
	   }
	   themeBtn.addEventListener('click', function() {
		   // Three-state cycle: auto -> light -> dark -> auto
		   const saved = localStorage.getItem('ofelia-theme') || 'auto';
		   let next;
		   if (saved === 'auto') next = 'light';
		   else if (saved === 'light') next = 'dark';
		   else next = 'auto';
		   setTheme(next, true);
	   });
	  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function() {
	    if ((localStorage.getItem('ofelia-theme') || 'auto') === 'auto') applyTheme();
	  });
	  applyTheme();
	})();
		(function () {
			const buttons = document.querySelectorAll('.cmd-expand-btn');
			buttons.forEach((btn) => {
				btn.addEventListener('click', () => {
					const target = document.getElementById(btn.dataset.target);
					if (!target) return;
					const expanded = btn.dataset.expanded === 'true';
					if (expanded) {
						target.classList.remove('expanded');
						btn.dataset.expanded = 'false';
						btn.setAttribute('aria-expanded', 'false');
						btn.textContent = 'Expand command';
					} else {
						target.classList.add('expanded');
						btn.dataset.expanded = 'true';
						btn.setAttribute('aria-expanded', 'true');
						btn.textContent = 'Collapse command';
					}
				});
			});

			const expandAllBtn = document.getElementById('expand-all-btn');
			if (!expandAllBtn) return;
			expandAllBtn.addEventListener('click', () => {
				const expandAll = expandAllBtn.dataset.expanded !== 'true';
				buttons.forEach((btn) => {
					const target = document.getElementById(btn.dataset.target);
					if (!target) return;
					if (expandAll) {
						target.classList.add('expanded');
						btn.dataset.expanded = 'true';
						btn.setAttribute('aria-expanded', 'true');
						btn.textContent = 'Collapse command';
					} else {
						target.classList.remove('expanded');
						btn.dataset.expanded = 'false';
						btn.setAttribute('aria-expanded', 'false');
						btn.textContent = 'Expand command';
					}
				});
				expandAllBtn.dataset.expanded = expandAll ? 'true' : 'false';
				expandAllBtn.textContent = expandAll ? 'Collapse all commands' : 'Expand all commands';
			});
		})();
	</script>
</body>
</html>`))
