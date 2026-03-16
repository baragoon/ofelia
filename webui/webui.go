package webui

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/baragoon/ofelia/cli"
	"github.com/baragoon/ofelia/core"
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

// ...existing code...

type webUIServer struct {
	server         *http.Server
	logger         core.Logger
	config         *cli.Config
	refreshSeconds int
}


// ...existing code...

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

       // Use reflection to access unexported fields (mu, sh, dockerHandler)
       var mu *sync.RWMutex
       var sh interface{ CronJobs() []cron.Entry }
       var dockerHandler interface{ GetInternalDockerClients() map[string]interface{} }
       if config != nil {
	       v := reflect.ValueOf(config).Elem()
	       muField := v.FieldByName("mu")
	       if muField.IsValid() && muField.CanAddr() {
		       mu = muField.Addr().Interface().(*sync.RWMutex)
	       }
	       shField := v.FieldByName("sh")
	       if shField.IsValid() && !shField.IsNil() {
		       sh = shField.Interface().(interface{ CronJobs() []cron.Entry })
	       }
	       dhField := v.FieldByName("dockerHandler")
	       if dhField.IsValid() && !dhField.IsNil() {
		       dockerHandler = dhField.Interface().(interface{ GetInternalDockerClients() map[string]interface{} })
	       }
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
