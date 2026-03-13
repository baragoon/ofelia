package cli

import (
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
