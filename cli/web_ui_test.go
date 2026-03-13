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
