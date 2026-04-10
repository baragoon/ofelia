package core

import (
	"archive/tar"
	"bytes"
	"context"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/fsouza/go-dockerclient/testing"
	logging "github.com/op/go-logging"

	. "gopkg.in/check.v1"
)

const ServiceImageFixture = "test-image"

type SuiteRunServiceJob struct {
	server *testing.DockerServer
	client *docker.Client
}

var _ = Suite(&SuiteRunServiceJob{})

const logFormat = "%{color}%{shortfile} ▶ %{level}%{color:reset} %{message}"

var logger Logger

func (s *SuiteRunServiceJob) SetUpTest(c *C) {
	var err error

	logging.SetFormatter(logging.MustStringFormatter(logFormat))

	logger = logging.MustGetLogger("ofelia")
	s.server, err = testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, IsNil)

	s.client, err = docker.NewClient(s.server.URL())
	c.Assert(err, IsNil)

	s.buildImage(c)
}

// mockSwarmClient is a test-double for swarmServiceClient that records the
// service spec it received and returns configurable task results.
type mockSwarmClient struct {
	createdSpec      swarm.ServiceSpec
	tasks            []swarm.Task
	removeCalled     bool
	removeCalledWith string
}

func (m *mockSwarmClient) ServiceCreate(_ context.Context, createOpts client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	m.createdSpec = createOpts.Spec
	return client.ServiceCreateResult{ID: "test-service-id"}, nil
}

func (m *mockSwarmClient) ServiceInspect(_ context.Context, serviceID string, _ client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	return client.ServiceInspectResult{
		Service: swarm.Service{
			ID:   serviceID,
			Meta: swarm.Meta{CreatedAt: time.Now()},
			Spec: m.createdSpec,
		},
	}, nil
}

func (m *mockSwarmClient) ServiceRemove(_ context.Context, serviceID string, _ client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	m.removeCalled = true
	m.removeCalledWith = serviceID
	return client.ServiceRemoveResult{}, nil
}

func (m *mockSwarmClient) TaskList(_ context.Context, _ client.TaskListOptions) (client.TaskListResult, error) {
	if len(m.tasks) == 0 {
		// Return a successfully completed task so watchContainer exits quickly.
		return client.TaskListResult{
			Items: []swarm.Task{
				{
					Status: swarm.TaskStatus{
						State:           swarm.TaskStateComplete,
						ContainerStatus: &swarm.ContainerStatus{ExitCode: 0},
					},
				},
			},
		}, nil
	}
	return client.TaskListResult{Items: m.tasks}, nil
}

func (s *SuiteRunServiceJob) TestRun(c *C) {
	mock := &mockSwarmClient{}

	job := &RunServiceJob{Client: s.client, mobyClient: mock}
	job.Image = ServiceImageFixture
	job.Command = `echo -a foo bar`
	job.User = "foo"
	job.TTY = true
	job.Delete = "true"
	job.Network = "foo"

	e := NewExecution()

	err := job.Run(&Context{Execution: e, Logger: logger})
	c.Assert(err, IsNil)

	// Verify the service spec was built correctly.
	cmd := mock.createdSpec.TaskTemplate.ContainerSpec.Command
	c.Assert(cmd, DeepEquals, []string{"echo", "-a", "foo", "bar"})

	nets := mock.createdSpec.TaskTemplate.Networks
	c.Assert(nets, HasLen, 1)
	c.Assert(nets[0].Target, Equals, "foo")

	// Verify the service was deleted when Delete="true".
	c.Assert(mock.removeCalled, Equals, true)
	c.Assert(mock.removeCalledWith, Equals, "test-service-id")
}

func (s *SuiteRunServiceJob) TestRunNoDelete(c *C) {
	mock := &mockSwarmClient{}

	job := &RunServiceJob{Client: s.client, mobyClient: mock}
	job.Image = ServiceImageFixture
	job.Delete = "false"

	e := NewExecution()

	err := job.Run(&Context{Execution: e, Logger: logger})
	c.Assert(err, IsNil)

	// Verify the service was NOT deleted when Delete="false".
	c.Assert(mock.removeCalled, Equals, false)
}

func (s *SuiteRunServiceJob) TestBuildPullImageOptionsBareImage(c *C) {
	o, _ := buildPullOptions("foo")
	c.Assert(o.Repository, Equals, "foo")
	c.Assert(o.Tag, Equals, "latest")
	c.Assert(o.Registry, Equals, "")
}

func (s *SuiteRunServiceJob) TestBuildPullImageOptionsVersion(c *C) {
	o, _ := buildPullOptions("foo:qux")
	c.Assert(o.Repository, Equals, "foo")
	c.Assert(o.Tag, Equals, "qux")
	c.Assert(o.Registry, Equals, "")
}

func (s *SuiteRunServiceJob) TestBuildPullImageOptionsRegistry(c *C) {
	o, _ := buildPullOptions("quay.io/srcd/rest:qux")
	c.Assert(o.Repository, Equals, "quay.io/srcd/rest")
	c.Assert(o.Tag, Equals, "qux")
	c.Assert(o.Registry, Equals, "quay.io")
}

func (s *SuiteRunServiceJob) buildImage(c *C) {
	inputbuf := bytes.NewBuffer(nil)
	tr := tar.NewWriter(inputbuf)
	dockerfile := []byte("FROM base\n")
	tr.WriteHeader(&tar.Header{Name: "Dockerfile", Size: int64(len(dockerfile))})
	tr.Write(dockerfile)
	tr.Close()

	err := s.client.BuildImage(docker.BuildImageOptions{
		Name:         ServiceImageFixture,
		InputStream:  inputbuf,
		OutputStream: bytes.NewBuffer(nil),
	})
	c.Assert(err, IsNil)
}
