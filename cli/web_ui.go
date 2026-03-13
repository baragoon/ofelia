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
}

type webUIHost struct {
	Key      string
	Title    string
	Jobs     []webUIJob
	JobCount int
}

type webUIState struct {
	Hosts      []webUIHost
	TotalHosts int
	TotalJobs  int
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

			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
				Name:     name,
				Type:     "exec",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   job.Container,
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

			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
				Name:     name,
				Type:     "run",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   target,
			})
		}

		for jobName, job := range config.ServiceJobs {
			host, name := splitHostPrefixedJobName(jobName)
			if strings.TrimSpace(job.DockerHost) != "" {
				host = job.DockerHost
			}

			getHost(host).Jobs = append(getHost(host).Jobs, webUIJob{
				Name:     name,
				Type:     "service-run",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   job.Image,
			})
		}

		for jobName, job := range config.LocalJobs {
			getHost(localHostKey).Jobs = append(getHost(localHostKey).Jobs, webUIJob{
				Name:     jobName,
				Type:     "local",
				Schedule: job.Schedule,
				Command:  job.Command,
				Target:   job.Dir,
			})
		}
	}

	hostList := make([]webUIHost, 0, len(hosts))
	totalJobs := 0
	for _, host := range hosts {
		sort.Slice(host.Jobs, func(i, j int) bool {
			if host.Jobs[i].Name != host.Jobs[j].Name {
				return host.Jobs[i].Name < host.Jobs[j].Name
			}
			return host.Jobs[i].Type < host.Jobs[j].Type
		})
		host.JobCount = len(host.Jobs)
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
		Hosts:      hostList,
		TotalHosts: len(hostList),
		TotalJobs:  totalJobs,
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

func hostTitle(key string) string {
	if key == localHostKey {
		return "Local Host"
	}

	return fmt.Sprintf("Docker Host %s", key)
}

func normalizeRefreshSeconds(refreshSeconds int) int {
	if refreshSeconds <= 0 {
		return defaultWebUIRefreshSeconds
	}

	return refreshSeconds
}

var hostsTemplate = template.Must(template.New("hosts").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="{{.RefreshSeconds}}">
  <title>Ofelia Hosts</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f5f8f2;
      --text: #1f2a1f;
      --surface: #ffffff;
      --accent: #1f7a57;
      --accent-soft: #e6f4ed;
      --muted: #6b7a6f;
      --border: #d5e1d8;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: var(--text);
      background:
        radial-gradient(circle at top right, #e9f5da 0%, transparent 40%),
        radial-gradient(circle at bottom left, #d8f0f0 0%, transparent 35%),
        var(--bg);
    }
    .wrap {
      max-width: 980px;
      margin: 0 auto;
      padding: 2rem 1rem 3rem;
    }
    .hero {
      background: linear-gradient(135deg, #ffffff, #f0f8f2);
      border: 1px solid var(--border);
      border-radius: 16px;
      padding: 1.2rem 1.4rem;
      margin-bottom: 1rem;
    }
    .hero h1 {
      margin: 0;
      font-size: 1.4rem;
    }
    .meta {
      margin-top: .4rem;
      color: var(--muted);
      font-size: .95rem;
    }
    .grid {
      display: grid;
      grid-template-columns: repeat(auto-fit, minmax(230px, 1fr));
      gap: .8rem;
      margin-top: 1rem;
    }
    .card {
      display: block;
      text-decoration: none;
      color: inherit;
      border: 1px solid var(--border);
      border-radius: 14px;
      background: var(--surface);
      padding: 1rem;
      transition: transform .12s ease, box-shadow .12s ease;
    }
    .card:hover {
      transform: translateY(-2px);
      box-shadow: 0 6px 20px rgba(31, 122, 87, .1);
      border-color: #b7d7c3;
    }
    .title {
      margin: 0 0 .3rem;
      font-weight: 700;
      font-size: 1rem;
      word-break: break-word;
    }
    .count {
      margin: 0;
      color: var(--muted);
      font-size: .95rem;
    }
    .badge {
      display: inline-block;
      margin-top: .6rem;
      font-size: .8rem;
      color: var(--accent);
      background: var(--accent-soft);
      border-radius: 999px;
      padding: .2rem .6rem;
      font-weight: 600;
    }
  </style>
</head>
<body>
  <main class="wrap">
    <section class="hero">
      <h1>Ofelia Scheduler UI</h1>
      <p class="meta">Hosts: {{.TotalHosts}} · Jobs: {{.TotalJobs}}</p>
    </section>
    <section class="grid">
      {{range .Hosts}}
      <a class="card" href="/hosts/{{.Key}}">
        <h2 class="title">{{.Title}}</h2>
        <p class="count">{{.JobCount}} job(s)</p>
        <span class="badge">Open Jobs</span>
      </a>
      {{end}}
    </section>
  </main>
</body>
</html>`))

var hostJobsTemplate = template.Must(template.New("host-jobs").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="{{.RefreshSeconds}}">
  <title>{{.Title}} Jobs</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f7f7fb;
      --text: #1f2030;
      --surface: #ffffff;
      --accent: #0f766e;
      --border: #d8dcec;
      --muted: #5f6475;
      --chip: #eef6f5;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: ui-sans-serif, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: var(--text);
      background:
        linear-gradient(180deg, #edf2fa, #f8fafc 25%),
        var(--bg);
    }
    .wrap {
      max-width: 1050px;
      margin: 0 auto;
      padding: 2rem 1rem 3rem;
    }
    .topbar {
      display: flex;
      justify-content: space-between;
      align-items: center;
      margin-bottom: .8rem;
      gap: .8rem;
      flex-wrap: wrap;
    }
    .topbar a {
      color: var(--accent);
      text-decoration: none;
      font-weight: 600;
    }
    .panel {
      border: 1px solid var(--border);
      border-radius: 14px;
      overflow: hidden;
      background: var(--surface);
    }
    table {
      width: 100%;
      border-collapse: collapse;
    }
    th, td {
      padding: .72rem .8rem;
      border-bottom: 1px solid var(--border);
      text-align: left;
      vertical-align: top;
      font-size: .92rem;
      word-break: break-word;
    }
    thead th {
      background: #f4f7fb;
      font-size: .78rem;
      letter-spacing: .03em;
      text-transform: uppercase;
      color: var(--muted);
    }
    tbody tr:hover { background: #fafcfe; }
    .chip {
      background: var(--chip);
      color: var(--accent);
      font-size: .78rem;
      border-radius: 999px;
      padding: .12rem .55rem;
      font-weight: 700;
    }
  </style>
</head>
<body>
  <main class="wrap">
    <div class="topbar">
      <div>
        <h1>{{.Title}}</h1>
        <p>{{.JobCount}} job(s)</p>
      </div>
      <a href="/">Back to hosts</a>
    </div>
    <section class="panel">
      <table>
        <thead>
          <tr>
            <th>Name</th>
            <th>Type</th>
            <th>Schedule</th>
            <th>Target</th>
            <th>Command</th>
          </tr>
        </thead>
        <tbody>
          {{range .Jobs}}
          <tr>
            <td>{{.Name}}</td>
            <td><span class="chip">{{.Type}}</span></td>
            <td>{{.Schedule}}</td>
            <td>{{.Target}}</td>
            <td>{{.Command}}</td>
          </tr>
          {{end}}
        </tbody>
      </table>
    </section>
  </main>
</body>
</html>`))
