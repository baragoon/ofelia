package cli

import (
	"strings"
	"testing"

	"github.com/baragoon/ofelia/core"
	docker "github.com/fsouza/go-dockerclient"
)

func TestSplitHostPrefixedJobName(t *testing.T) {
	testCases := []struct {
		name         string
		input        string
		expectedHost string
		expectedJob  string
	}{
		{name: "prefixed", input: "docker-a::backup", expectedHost: "docker-a", expectedJob: "backup"},
		{name: "not prefixed", input: "cleanup", expectedHost: localHostKey, expectedJob: "cleanup"},
		{name: "empty host", input: "::job", expectedHost: localHostKey, expectedJob: "job"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			host, job := splitHostPrefixedJobName(tc.input)
			if host != tc.expectedHost {
				t.Fatalf("expected host %q, got %q", tc.expectedHost, host)
			}

			if job != tc.expectedJob {
				t.Fatalf("expected job %q, got %q", tc.expectedJob, job)
			}
		})
	}
}

func TestTrimContainerPrefixedJobName(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "container prefixed", input: "adguardhome::datecron", expected: "datecron"},
		{name: "not prefixed", input: "datecron", expected: "datecron"},
		{name: "empty job part", input: "adguardhome::", expected: "adguardhome::"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimContainerPrefixedJobName(tc.input); got != tc.expected {
				t.Fatalf("expected %q, got %q", tc.expected, got)
			}
		})
	}
}

func TestIsMultilineCommand(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected bool
	}{
		{name: "newline", input: "echo one\necho two", expected: true},
		{name: "line continuation", input: "curl foo \\ -H bar", expected: true},
		{name: "single line", input: "date", expected: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMultilineCommand(tc.input); got != tc.expected {
				t.Fatalf("expected %t, got %t", tc.expected, got)
			}
		})
	}
}

func TestHostTitle(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{name: "numeric suffix converts to port", input: "docker_3000", expected: "docker:3000"},
		{name: "non numeric suffix unchanged", input: "docker_abc", expected: "docker_abc"},
		{name: "local host title", input: "local", expected: "Local Host"},
		{name: "leading underscore unchanged", input: "_3000", expected: "_3000"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := hostTitle(tc.input)
			if got != tc.expected {
				t.Errorf("hostTitle(%q): expected %q, got %q", tc.input, tc.expected, got)
			}
		})
	}
}

func TestBuildWebUIStateGroupsJobsByHost(t *testing.T) {
	config := NewConfig(&TestLogger{})
	config.dockerHandler = &DockerHandler{}
	config.dockerHandler.dockerClients = map[string]*docker.Client{}
	config.dockerHandler.dockerClients["docker-a"] = nil
	config.dockerHandler.dockerClients["docker-b"] = nil

	config.ExecJobs["docker-a::job-a"] = &ExecJobConfig{ExecJob: core.ExecJob{BareJob: core.BareJob{Schedule: "@every 1m", Command: "echo a"}, Container: "container-a"}}
	config.RunJobs["job-b"] = &RunJobConfig{RunJob: core.RunJob{BareJob: core.BareJob{Schedule: "@every 2m", Command: "echo b"}, Image: "alpine"}, DockerHost: "docker-b"}
	config.LocalJobs["local-job"] = &LocalJobConfig{LocalJob: core.LocalJob{BareJob: core.BareJob{Schedule: "@every 3m", Command: "date"}, Dir: "/tmp"}}

	state := buildWebUIState(config)

	if state.TotalHosts != 3 {
		t.Fatalf("expected 3 hosts, got %d", state.TotalHosts)
	}

	if state.TotalJobs != 3 {
		t.Fatalf("expected 3 jobs, got %d", state.TotalJobs)
	}

	hosts := map[string]webUIHost{}
	for _, host := range state.Hosts {
		hosts[host.Key] = host
	}

	if hosts[localHostKey].JobCount != 1 {
		t.Fatalf("expected local host to have 1 job, got %d", hosts[localHostKey].JobCount)
	}

	if hosts["docker-a"].JobCount != 1 {
		t.Fatalf("expected docker-a host to have 1 job, got %d", hosts["docker-a"].JobCount)
	}

	if hosts["docker-b"].JobCount != 1 {
		t.Fatalf("expected docker-b host to have 1 job, got %d", hosts["docker-b"].JobCount)
	}
}

func TestNormalizeRefreshSeconds(t *testing.T) {
	testCases := []struct {
		name     string
		in       int
		expected int
	}{
		{name: "positive", in: 7, expected: 7},
		{name: "zero fallback", in: 0, expected: defaultWebUIRefreshSeconds},
		{name: "negative fallback", in: -5, expected: defaultWebUIRefreshSeconds},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRefreshSeconds(tc.in); got != tc.expected {
				t.Fatalf("expected %d, got %d", tc.expected, got)
			}
		})
	}
}

func TestMaskSecrets(t *testing.T) {
	testCases := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "url query password",
			input:    "mysql://user:pass@host/db?password=s3cr3t",
			expected: "mysql://user:pass@host/db?password=***",
		},
		{
			name:     "http header api key",
			input:    `curl -H "Api-Key: abc123" https://example.com`,
			expected: `curl -H "Api-Key: ***" https://example.com`,
		},
		{
			name:     "authorization header",
			input:    "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9",
			expected: "Authorization: ***",
		},
		{
			name:     "env var token",
			input:    "docker run -e token=secret123 alpine",
			expected: "docker run -e token=*** alpine",
		},
		{
			name:     "no secrets",
			input:    "echo hello world",
			expected: "echo hello world",
		},
		{
			name:     "secret= assignment",
			input:    "export SECRET=mypassphrase",
			expected: "export SECRET=***",
		},
		{
			name:     "multiple secrets in one input",
			input:    "docker run -e token=secret123 -e password=hunter2 alpine",
			expected: "docker run -e token=*** -e password=*** alpine",
		},
		{
			name:     "case insensitive password variants",
			input:    "PASSWORD=topsecret Password=midsecret password=lowsecret",
			expected: "PASSWORD=*** Password=*** password=***",
		},
		{
			name:     "secret at end of string",
			input:    "echo token=secret123",
			expected: "echo token=***",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := maskSecrets(tc.input)
			if got != tc.expected {
				t.Errorf("maskSecrets(%q)\n  want: %q\n   got: %q", tc.input, tc.expected, got)
			}
		})
	}
}

func TestOutputContainsHTTPFailure(t *testing.T) {
	testCases := []struct {
		name   string
		output string
		want   bool
	}{
		{name: "200 ok", output: "HTTP/1.1 200 OK\nContent-Length: 0", want: false},
		{name: "404 not found", output: "HTTP/1.1 404 Not Found\n", want: true},
		{name: "500 server error", output: "HTTP/2 500 Internal Server Error\n", want: true},
		{name: "301 redirect", output: "HTTP/1.1 301 Moved Permanently\n", want: true},
		{name: "no http response", output: "some random job output\nwith multiple lines", want: false},
		{name: "multiple responses last fails", output: "HTTP/1.1 200 OK\nHTTP/1.1 404 Not Found\n", want: true},
		{name: "201 created is success", output: "HTTP/1.1 201 Created\n", want: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := outputContainsHTTPFailure(tc.output)
			if got != tc.want {
				t.Errorf("outputContainsHTTPFailure(%q): want %v, got %v", tc.output, tc.want, got)
			}
		})
	}
}

func TestBuildWebUIStateLastRunFields(t *testing.T) {
	config := NewConfig(&TestLogger{})
	config.dockerHandler = &DockerHandler{}
	config.dockerHandler.dockerClients = map[string]*docker.Client{}
	findJobByName := func(state webUIState, name string) (webUIJob, bool) {
		for _, host := range state.Hosts {
			for _, job := range host.Jobs {
				if job.Name == name {
					return job, true
				}
			}
		}

		return webUIJob{}, false
	}

	job := &ExecJobConfig{ExecJob: core.ExecJob{BareJob: core.BareJob{Schedule: "@every 1m", Command: "echo hello"}, Container: "c1"}}
	config.ExecJobs["myjob"] = job

	// Before any execution: LastExitOK should be nil
	state := buildWebUIState(config)
	j, ok := findJobByName(state, "myjob")
	if !ok {
		t.Fatal("expected to find job named myjob in state")
	}
	if j.LastExitOK != nil {
		t.Errorf("expected LastExitOK nil before any run, got %v", *j.LastExitOK)
	}
	if j.LastOutput != "" {
		t.Errorf("expected empty LastOutput before any run, got %q", j.LastOutput)
	}

	// Simulate a successful execution
	exec := core.NewExecution()
	exec.Start()
	_, _ = exec.OutputStream.Write([]byte("hello"))
	exec.Stop(nil)
	job.ExecJob.BareJob.SetLastExecution(exec)

	state = buildWebUIState(config)
	j, ok = findJobByName(state, "myjob")
	if !ok {
		t.Fatal("expected to find job named myjob in state after successful execution")
	}
	if j.LastExitOK == nil {
		t.Fatal("expected LastExitOK non-nil after a run")
	}
	if !*j.LastExitOK {
		t.Errorf("expected LastExitOK true for successful execution")
	}
	if j.LastOutput != "hello" {
		t.Errorf("expected LastOutput %q, got %q", "hello", j.LastOutput)
	}
	if j.LastRun.IsZero() {
		t.Errorf("expected LastRun to be set after execution")
	}

	// Simulate a failed execution
	exec2 := core.NewExecution()
	exec2.Start()
	_, _ = exec2.ErrorStream.Write([]byte("exit 1"))
	exec2.Stop(core.ErrUnexpected)
	job.ExecJob.BareJob.SetLastExecution(exec2)

	state = buildWebUIState(config)
	j, ok = findJobByName(state, "myjob")
	if !ok {
		t.Fatal("expected to find job named myjob in state after failed execution")
	}
	if j.LastExitOK == nil {
		t.Fatal("expected LastExitOK non-nil after a failed run")
	}
	if *j.LastExitOK {
		t.Errorf("expected LastExitOK false for failed execution")
	}
}

func TestBuildWebUIStateCommandMasking(t *testing.T) {
	config := NewConfig(&TestLogger{})
	config.dockerHandler = &DockerHandler{}
	config.dockerHandler.dockerClients = map[string]*docker.Client{}

	config.ExecJobs["secret-job"] = &ExecJobConfig{
		ExecJob: core.ExecJob{BareJob: core.BareJob{Schedule: "@every 1m", Command: `curl -H "Api-Key: topsecret" https://api.example.com`}, Container: "c1"},
	}

	state := buildWebUIState(config)
	if len(state.Hosts) == 0 || len(state.Hosts[0].Jobs) == 0 {
		t.Fatal("expected job in state")
	}
	cmd := state.Hosts[0].Jobs[0].Command
	if strings.Contains(cmd, "topsecret") {
		t.Errorf("command should have API key masked, got: %q", cmd)
	}
	if !strings.Contains(cmd, "***") {
		t.Errorf("command should contain *** mask, got: %q", cmd)
	}
}
