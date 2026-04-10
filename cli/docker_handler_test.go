package cli

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"

	"github.com/baragoon/ofelia/core"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/fsouza/go-dockerclient/testing"
	check "gopkg.in/check.v1"
)

var _ = check.Suite(&TestDockerSuit{})

const imageFixture = "ofelia/test-image"

type TestDockerSuit struct {
	server *testing.DockerServer
	client *docker.Client
}

func buildFromDockerLabels(dockerFilters ...string) (*Config, error) {
	mockLogger := &TestLogger{}
	c := &Config{
		sh: core.NewScheduler(mockLogger),
	}

	var err error
	c.dockerHandler, err = NewDockerHandler(c, dockerFilters, true, mockLogger, DockerConnectionOptions{})
	if err != nil {
		return nil, err
	}
	dockerLabels, err := c.dockerHandler.GetDockerLabels()
	if err != nil {
		return nil, err
	}

	c.buildFromDockerLabels(dockerLabels)
	return c, nil
}

func (s *TestDockerSuit) SetUpTest(c *check.C) {
	var err error
	s.server, err = testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)

	s.client, err = docker.NewClient(s.server.URL())
	c.Assert(err, check.IsNil)

	err = core.BuildTestImage(s.client, imageFixture)
	c.Assert(err, check.IsNil)

	os.Setenv("DOCKER_HOST", s.server.URL())
}

func (s *TestDockerSuit) TearDownTest(c *check.C) {
	os.Unsetenv("DOCKER_HOST")
	clearPatternedDockerHostEnv()
}

func (s *TestDockerSuit) TestResolveConfiguredDockerHostsPrefersCLI(c *check.C) {
	os.Setenv("DOCKER_HOST_0", "tcp://env1:2376")
	os.Setenv("DOCKER_HOST_1", "tcp://env2:2376")
	os.Setenv("DOCKER_HOST", "tcp://legacy:2375")

	hosts := resolveConfiguredDockerHosts([]string{"tcp://cli1:2376", "tcp://cli2:2376"})
	c.Assert(hosts, check.DeepEquals, []string{"tcp://cli1:2376", "tcp://cli2:2376"})
}

func (s *TestDockerSuit) TestResolveConfiguredDockerHostsFromPatternedEnv(c *check.C) {
	os.Unsetenv("DOCKER_HOST")
	os.Setenv("DOCKER_HOST_10", "tcp://dind10:2376")
	os.Setenv("DOCKER_HOST_2", "tcp://dind2:2376")
	os.Setenv("DOCKER_HOST_ALPHA", "tcp://alpha:2376")
	os.Setenv("DOCKER_HOST.BETA", "tcp://beta:2376")

	hosts := resolveConfiguredDockerHosts(nil)
	c.Assert(hosts, check.DeepEquals, []string{
		"tcp://dind2:2376",
		"tcp://dind10:2376",
		"tcp://alpha:2376",
		"tcp://beta:2376",
	})
}

func (s *TestDockerSuit) TestResolveConfiguredDockerHostsFromBracketedIndexedEnv(c *check.C) {
	os.Unsetenv("DOCKER_HOST")
	os.Setenv("DOCKER_HOST.[1]", "tcp://dind2:2376")
	os.Setenv("DOCKER_HOST.[0]", "tcp://dind1:2376")

	hosts := resolveConfiguredDockerHosts(nil)
	c.Assert(hosts, check.DeepEquals, []string{"tcp://dind1:2376", "tcp://dind2:2376"})
}

func (s *TestDockerSuit) TestResolveConfiguredDockerHostsFromDockerHostList(c *check.C) {
	os.Setenv("DOCKER_HOST", "tcp://dind1:2376,tcp://dind2:2376")

	hosts := resolveConfiguredDockerHosts(nil)
	c.Assert(hosts, check.DeepEquals, []string{"tcp://dind1:2376", "tcp://dind2:2376"})
}

func (s *TestDockerSuit) TestResolveConfiguredDockerHostsSingleDockerHostKeepsLegacyFlow(c *check.C) {
	os.Setenv("DOCKER_HOST", "tcp://single-host:2375")

	hosts := resolveConfiguredDockerHosts(nil)
	c.Assert(hosts, check.IsNil)
}

func clearPatternedDockerHostEnv() {
	for _, rawEnv := range os.Environ() {
		key, _, ok := strings.Cut(rawEnv, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, dockerHostIndexedEnvPrefix) || strings.HasPrefix(key, dockerHostDottedEnvPrefix) {
			os.Unsetenv(key)
		}
	}
}

func (s *TestDockerSuit) TestLabelsFilterJobsCount(c *check.C) {
	filterLabel := []string{"test_filter_label", "yesssss"}
	containersToStartWithLabels := []map[string]string{
		{
			requiredLabel:  "true",
			filterLabel[0]: filterLabel[1],
			labelPrefix + "." + jobExec + ".job2.schedule":  "* * * * *",
			labelPrefix + "." + jobExec + ".job2.command":   "command2",
			labelPrefix + "." + jobExec + ".job2.container": "container2",
		},
		{
			requiredLabel: "true",
			labelPrefix + "." + jobExec + ".job3.schedule":  "* * * * *",
			labelPrefix + "." + jobExec + ".job3.command":   "command3",
			labelPrefix + "." + jobExec + ".job3.container": "container3",
		},
	}

	_, err := s.startTestContainersWithLabels(containersToStartWithLabels)
	c.Assert(err, check.IsNil)

	conf, err := buildFromDockerLabels("label=" + strings.Join(filterLabel, "="))
	c.Assert(err, check.IsNil)
	c.Assert(conf.sh, check.NotNil)

	c.Assert(conf.JobsCount(), check.Equals, 1)
}

func (s *TestDockerSuit) TestFilterErrorsLabel(c *check.C) {
	containersToStartWithLabels := []map[string]string{
		{
			labelPrefix + "." + jobExec + ".job2.schedule": "schedule2",
			labelPrefix + "." + jobExec + ".job2.command":  "command2",
		},
	}

	_, err := s.startTestContainersWithLabels(containersToStartWithLabels)
	c.Assert(err, check.IsNil)

	{
		conf, err := buildFromDockerLabels()
		c.Assert(strings.Contains(err.Error(), requiredLabelFilter), check.Equals, true)
		c.Assert(conf, check.IsNil)
	}

	customLabelFilter := []string{"label", "test=123"}
	{
		conf, err := buildFromDockerLabels(strings.Join(customLabelFilter, "="))
		c.Assert(errors.Is(err, errNoContainersMatchingFilters), check.Equals, true)
		c.Assert(err, check.ErrorMatches, fmt.Sprintf(`.*%s:.*%s.*`, "label", requiredLabel))
		c.Assert(err, check.ErrorMatches, fmt.Sprintf(`.*%s:.*%s.*`, customLabelFilter[0], customLabelFilter[1]))
		c.Assert(conf, check.IsNil)
	}

	{
		customNameFilter := []string{"name", "test-name"}
		conf, err := buildFromDockerLabels(strings.Join(customLabelFilter, "="), strings.Join(customNameFilter, "="))
		c.Assert(errors.Is(err, errNoContainersMatchingFilters), check.Equals, true)
		c.Assert(err, check.ErrorMatches, fmt.Sprintf(`.*%s:.*%s.*`, "label", requiredLabel))
		c.Assert(err, check.ErrorMatches, fmt.Sprintf(`.*%s:.*%s.*`, customLabelFilter[0], customLabelFilter[1]))
		c.Assert(err, check.ErrorMatches, fmt.Sprintf(`.*%s:.*%s.*`, customNameFilter[0], customNameFilter[1]))
		c.Assert(conf, check.IsNil)
	}

	{
		customBadFilter := "label-test"
		conf, err := buildFromDockerLabels(customBadFilter)
		c.Assert(errors.Is(err, errInvalidDockerFilter), check.Equals, true)
		c.Assert(conf, check.IsNil)
	}
}

func (s *TestDockerSuit) TestExecJobMultilineCommandFromLabels(c *check.C) {
	multilineCommand := "sh -lc echo first-line\nprintf second-line"
	containersToStartWithLabels := []map[string]string{
		{
			requiredLabel: "true",
			labelPrefix + "." + jobExec + ".job1.schedule": "* * * * *",
			labelPrefix + "." + jobExec + ".job1.command":  multilineCommand,
		},
	}

	_, err := s.startTestContainersWithLabels(containersToStartWithLabels)
	c.Assert(err, check.IsNil)

	conf, err := buildFromDockerLabels()
	c.Assert(err, check.IsNil)
	c.Assert(len(conf.ExecJobs), check.Equals, 1)

	var job *ExecJobConfig
	for _, candidate := range conf.ExecJobs {
		job = candidate
	}
	c.Assert(job, check.NotNil)
	c.Assert(job.Command, check.Equals, multilineCommand)

	job.Client = s.client
	err = job.Run(&core.Context{Execution: core.NewExecution()})
	c.Assert(err, check.IsNil)

	container, err := s.client.InspectContainer(job.Container)
	c.Assert(err, check.IsNil)
	c.Assert(len(container.ExecIDs) > 0, check.Equals, true)

	exec, err := s.client.InspectExec(container.ExecIDs[len(container.ExecIDs)-1])
	c.Assert(err, check.IsNil)
	c.Assert(exec.ProcessConfig.EntryPoint, check.Equals, "sh")
	c.Assert(exec.ProcessConfig.Arguments, check.DeepEquals, []string{"-lc", "echo first-line\nprintf second-line"})
}

func (s *TestDockerSuit) TestExecJobNameIncludesContainerForRemoteHost(c *check.C) {
	containersToStartWithLabels := []map[string]string{
		{
			requiredLabel: "true",
			labelPrefix + "." + jobExec + ".datecron.schedule": "* * * * *",
			labelPrefix + "." + jobExec + ".datecron.command":  "uname -a",
		},
	}

	_, err := s.startTestContainersWithLabels(containersToStartWithLabels)
	c.Assert(err, check.IsNil)

	conf, err := buildFromDockerLabels()
	c.Assert(err, check.IsNil)
	c.Assert(len(conf.ExecJobs), check.Equals, 1)

	for name := range conf.ExecJobs {
		c.Assert(strings.Contains(name, "::ofelia-test0::datecron"), check.Equals, true)
	}
}

func (s *TestDockerSuit) TestGetDockerLabelsSkipsFailedHosts(c *check.C) {
	containersToStartWithLabels := []map[string]string{
		{
			requiredLabel: "true",
			labelPrefix + "." + jobExec + ".datecron.schedule": "* * * * *",
			labelPrefix + "." + jobExec + ".datecron.command":  "date",
		},
	}

	_, err := s.startTestContainersWithLabels(containersToStartWithLabels)
	c.Assert(err, check.IsNil)

	forbiddenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer forbiddenServer.Close()

	forbiddenClient, err := docker.NewClient(forbiddenServer.URL)
	c.Assert(err, check.IsNil)

	h := &DockerHandler{
		dockerClients: map[string]*docker.Client{
			"bad-host":  forbiddenClient,
			"good-host": s.client,
		},
		logger: &TestLogger{},
	}

	labels, err := h.GetDockerLabels()
	c.Assert(err, check.IsNil)
	c.Assert(len(labels) > 0, check.Equals, true)

	foundGoodHost := false
	for containerRef := range labels {
		host, _ := splitContainerRef(containerRef)
		if host == "good-host" {
			foundGoodHost = true
			break
		}
	}
	c.Assert(foundGoodHost, check.Equals, true)
}

func (s *TestDockerSuit) TestGetDockerLabelsAllHostsFailed(c *check.C) {
	forbiddenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer forbiddenServer.Close()

	forbiddenClient, err := docker.NewClient(forbiddenServer.URL)
	c.Assert(err, check.IsNil)

	h := &DockerHandler{
		dockerClients: map[string]*docker.Client{
			"bad-host": forbiddenClient,
		},
		logger: &TestLogger{},
	}

	labels, err := h.GetDockerLabels()
	c.Assert(labels, check.IsNil)
	c.Assert(errors.Is(err, errFailedToListContainers), check.Equals, true)
}

func (s *TestDockerSuit) TestGetDockerLabelsNoClientsConfigured(c *check.C) {
	h := &DockerHandler{
		dockerClients: map[string]*docker.Client{},
		logger:        &TestLogger{},
	}

	labels, err := h.GetDockerLabels()
	c.Assert(labels, check.IsNil)
	c.Assert(errors.Is(err, errFailedToListContainers), check.Equals, true)
	c.Assert(err.Error(), check.Matches, ".*no docker clients configured.*")
}

func (s *TestDockerSuit) startTestContainersWithLabels(containerLabels []map[string]string) ([]*docker.Container, error) {
	containers := []*docker.Container{}

	for i := range containerLabels {
		cont, err := s.client.CreateContainer(docker.CreateContainerOptions{
			Name: fmt.Sprintf("ofelia-test%d", i),
			Config: &docker.Config{
				Cmd:    []string{"sleep", "500"},
				Labels: containerLabels[i],
				Image:  imageFixture,
			},
		})
		if err != nil {
			return containers, err
		}

		containers = append(containers, cont)
		if err := s.client.StartContainer(cont.ID, nil); err != nil {
			return containers, err
		}
	}

	return containers, nil
}

func (s *TestDockerSuit) TestGetContainerID(c *check.C) {
		// Set env to allow any mountinfo file for test
		origEnv := os.Getenv("OFELIA_TEST_ALLOW_ANY_MOUNTINFO")
		os.Setenv("OFELIA_TEST_ALLOW_ANY_MOUNTINFO", "1")
		defer os.Setenv("OFELIA_TEST_ALLOW_ANY_MOUNTINFO", origEnv)
	tests := []struct {
		content string
		expect  string
	}{
		{
			content: `
206 205 0:29 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw
207 203 0:67 / /dev/mqueue rw,nosuid,nodev,noexec,relatime - mqueue mqueue rw
208 203 0:72 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
209 201 254:1 /docker/containers/test123/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/vda1 rw,discard
210 201 254:1 /docker/containers/test123/hostname /etc/hostname rw,relatime - ext4 /dev/vda1 rw,discard
211 201 254:1 /docker/containers/test123/hosts /etc/hosts rw,relatime - ext4 /dev/vda1 rw,discard
85 203 0:70 /0 /dev/console rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=666
86 202 0:68 /bus /proc/bus ro,nosuid,nodev,noexec,relatime - proc proc rw
87 202 0:68 /fs /proc/fs ro,nosuid,nodev,noexec,relatime - proc proc rw
88 202 0:68 /irq /proc/irq ro,nosuid,nodev,noexec,relatime - proc proc rw
`,
			expect: "test123",
		},
		{
			content: `
206 205 0:29 / /sys/fs/cgroup ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw
207 203 0:67 / /dev/mqueue rw,nosuid,nodev,noexec,relatime - mqueue mqueue rw
208 203 0:72 / /dev/shm rw,nosuid,nodev,noexec,relatime - tmpfs shm rw,size=65536k
209 201 254:1 /var/lib/docker/containers/test123/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/vda1 rw,discard
210 201 254:1 /var/lib/docker/containers/test123/hostname /etc/hostname rw,relatime - ext4 /dev/vda1 rw,discard
211 201 254:1 /var/lib/docker/containers/test123/hosts /etc/hosts rw,relatime - ext4 /dev/vda1 rw,discard
85 203 0:70 /0 /dev/console rw,nosuid,noexec,relatime - devpts devpts rw,gid=5,mode=620,ptmxmode=666
86 202 0:68 /bus /proc/bus ro,nosuid,nodev,noexec,relatime - proc proc rw
87 202 0:68 /fs /proc/fs ro,nosuid,nodev,noexec,relatime - proc proc rw
88 202 0:68 /irq /proc/irq ro,nosuid,nodev,noexec,relatime - proc proc rw
`,
			expect: "test123",
		},
	}

	for _, tt := range tests {
		tmpFile, _ := os.CreateTemp(os.TempDir(), "mountinfo")
		tmpFile.WriteString(tt.content)
		defer os.Remove(tmpFile.Name())

		id, err := getContainerID(tmpFile.Name())
		c.Assert(err, check.IsNil)
		c.Assert(id, check.Equals, tt.expect)
	}
}

func (s *TestDockerSuit) TestNewDockerHandlerSkipsUnreachableHosts(c *check.C) {
	conf := &Config{}

	h, err := NewDockerHandler(conf, nil, false, &TestLogger{}, DockerConnectionOptions{
		Hosts: []string{"tcp://127.0.0.1:1", s.server.URL()},
	})
	c.Assert(err, check.IsNil)
	c.Assert(h, check.NotNil)

	clients := h.GetInternalDockerClients()
	c.Assert(len(clients), check.Equals, 1)

	goodHostKey := normalizeDockerHostKey(s.server.URL())
	_, ok := clients[goodHostKey]
	c.Assert(ok, check.Equals, true)
	c.Assert(h.GetPrimaryDockerHost(), check.Equals, goodHostKey)
}

func (s *TestDockerSuit) TestShouldUseTLSForHost(c *check.C) {
	tests := []struct {
		name      string
		host      string
		globalTLS bool
		expected  bool
	}{
		{name: "global TLS for tcp 2376", host: "tcp://secure.example:2376", globalTLS: true, expected: true},
		{name: "force TLS for tcp 2376 even when global is off", host: "tcp://secure.example:2376", globalTLS: false, expected: true},
		{name: "disable TLS for tcp 2375", host: "tcp://socket-proxy:2375", globalTLS: true, expected: false},
		{name: "disable TLS for explicit http", host: "http://socket-proxy:2375", globalTLS: true, expected: false},
		{name: "force TLS for explicit https", host: "https://secure.example:2376", globalTLS: false, expected: true},
		{name: "fallback to global value for unknown scheme", host: "ssh://remote-docker", globalTLS: true, expected: true},
		{name: "fallback to global value for no scheme", host: "remote-docker:2376", globalTLS: false, expected: false},
	}

	for _, tt := range tests {
		c.Assert(shouldUseTLSForHost(tt.host, tt.globalTLS), check.Equals, tt.expected, check.Commentf(tt.name))
	}
}
