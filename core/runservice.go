package core

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

const (
	// TODO are these const defined somewhere in the docker API?
	swarmError = -999
)

// swarmServiceClient is the minimal interface for Docker swarm service and task
// operations. Using an interface rather than the concrete *client.Client allows
// tests to inject a mock without requiring a live Docker daemon.
type swarmServiceClient interface {
	ServiceCreate(ctx context.Context, options client.ServiceCreateOptions) (client.ServiceCreateResult, error)
	ServiceInspect(ctx context.Context, serviceID string, options client.ServiceInspectOptions) (client.ServiceInspectResult, error)
	ServiceRemove(ctx context.Context, serviceID string, options client.ServiceRemoveOptions) (client.ServiceRemoveResult, error)
	TaskList(ctx context.Context, options client.TaskListOptions) (client.TaskListResult, error)
}

type RunServiceJob struct {
	BareJob    `mapstructure:",squash"`
	Client     *docker.Client    `json:"-"`
	mobyClient swarmServiceClient `json:"-"`
	User       string            `default:"root"`
	TTY        bool              `default:"false"`
	// do not use bool values with "default:true" because if
	// user would set it to "false" explicitly, it still will be
	// changed to "true" https://github.com/baragoon/ofelia/issues/135
	// so lets use strings here as workaround
	Delete  string `default:"true"`
	Image   string
	Network string
}

func NewRunServiceJob(c *docker.Client) *RunServiceJob {
	return &RunServiceJob{Client: c}
}

// getMobyClient initializes or returns the cached swarm service client.
// The endpoint URL is normalised to the tcp:// scheme expected by the moby
// client so that the URL is parsed correctly (in particular, trailing slashes
// in http:// URLs are stripped). TLS is configured from the standard Docker
// environment variables when the endpoint conventionally requires it.
func (j *RunServiceJob) getMobyClient() (swarmServiceClient, error) {
	if j.mobyClient != nil {
		return j.mobyClient, nil
	}

	endpoint := j.Client.Endpoint()
	// Convert http/https endpoint URLs to tcp:// so that moby's ParseHostURL
	// correctly extracts host:port (stripping any trailing path component).
	mobyEndpoint := toMobyEndpoint(endpoint)

	opts := []client.Opt{
		client.WithHost(mobyEndpoint),
		client.WithAPIVersionNegotiation(),
	}

	// Apply TLS configuration from environment when the endpoint uses a TLS
	// port or scheme (port 2376 or https://).
	if needsTLSForEndpoint(endpoint) {
		opts = append(opts, client.WithTLSClientConfigFromEnv())
	}

	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create moby client: %w", err)
	}
	j.mobyClient = c
	return c, nil
}

// toMobyEndpoint converts an http/https Docker endpoint URL to the tcp://
// format that the moby client handles correctly (it strips any trailing path).
func toMobyEndpoint(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" {
		return endpoint
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return "tcp://" + u.Host
	}
	return endpoint
}

// needsTLSForEndpoint reports whether the given Docker endpoint should use TLS.
// Port 2376 and the https:// scheme conventionally indicate TLS endpoints.
func needsTLSForEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" {
		return os.Getenv("DOCKER_TLS_VERIFY") != ""
	}
	switch u.Scheme {
	case "https":
		return true
	case "http", "unix", "npipe":
		return false
	case "tcp":
		return u.Port() == "2376"
	}
	return os.Getenv("DOCKER_TLS_VERIFY") != ""
}

func (j *RunServiceJob) Run(ctx *Context) error {
	if err := j.pullImage(); err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(context.Background(), maxProcessDuration)
	defer cancel()

	svc, err := j.buildService(runCtx)
	if err != nil {
		return err
	}

	ctx.Logger.Noticef("Created service %s for job %s\n", svc.ID, j.Name)

	if err := j.watchContainer(ctx, runCtx, svc.ID); err != nil {
		return err
	}

	return j.deleteService(ctx, runCtx, svc.ID)
}

func (j *RunServiceJob) pullImage() error {
	o, a := buildPullOptions(j.Image)
	if err := j.Client.PullImage(o, a); err != nil {
		return fmt.Errorf("error pulling image %q: %s", j.Image, err)
	}

	return nil
}

func (j *RunServiceJob) buildService(ctx context.Context) (*swarm.Service, error) {
	mc, err := j.getMobyClient()
	if err != nil {
		return nil, err
	}

	max := uint64(1)
	spec := swarm.ServiceSpec{
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image: j.Image,
			},
			RestartPolicy: &swarm.RestartPolicy{
				MaxAttempts: &max,
				Condition:   swarm.RestartPolicyConditionNone,
			},
		},
	}

	// For a service to interact with other services in a stack,
	// we need to attach it to the same network.
	if j.Network != "" {
		spec.TaskTemplate.Networks = []swarm.NetworkAttachmentConfig{
			{Target: j.Network},
		}
	}

	if j.Command != "" {
		spec.TaskTemplate.ContainerSpec.Command = strings.Split(j.Command, " ")
	}

	result, err := mc.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec})
	if err != nil {
		return nil, err
	}

	// Fetch the created service to return a fully populated struct.
	inspectResult, err := mc.ServiceInspect(ctx, result.ID, client.ServiceInspectOptions{})
	if err != nil {
		return nil, err
	}

	return &inspectResult.Service, nil
}

func (j *RunServiceJob) watchContainer(ctx *Context, runCtx context.Context, svcID string) error {
	mc, err := j.getMobyClient()
	if err != nil {
		return fmt.Errorf("failed to get moby client: %w", err)
	}

	exitCode := swarmError

	ctx.Logger.Noticef("Checking for service ID %s (%s) termination\n", svcID, j.Name)

	inspectResult, err := mc.ServiceInspect(runCtx, svcID, client.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("Failed to inspect service %s: %s", svcID, err.Error())
	}
	svc := &inspectResult.Service

	// Create a per-invocation ticker so it can be stopped when the goroutine
	// exits, avoiding a resource leak that would occur with a package-level ticker.
	ticker := time.NewTicker(watchDuration)
	defer ticker.Stop()

	// On every tick, check if all the tasks have completed or errored out.
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		for {
			select {
			case <-runCtx.Done():
				err = ErrMaxTimeRunning
				return
			case <-ticker.C:
				if time.Now().After(svc.CreatedAt.Add(maxProcessDuration)) {
					err = ErrMaxTimeRunning
					return
				}

				taskExitCode, found := j.findtaskstatus(ctx, runCtx, svc.ID)
				if found {
					exitCode = taskExitCode
					return
				}
			}
		}
	}()

	wg.Wait()

	ctx.Logger.Noticef("Service ID %s (%s) has completed with exit code %d\n", svcID, j.Name, exitCode)
	return err
}

func (j *RunServiceJob) findtaskstatus(ctx *Context, runCtx context.Context, taskID string) (int, bool) {
	mc, err := j.getMobyClient()
	if err != nil {
		ctx.Logger.Errorf("Failed to get moby client for task lookup: %s\n", err.Error())
		return 0, false
	}

	filters := make(client.Filters)
	filters.Add("service", taskID)

	result, err := mc.TaskList(runCtx, client.TaskListOptions{Filters: filters})
	if err != nil {
		ctx.Logger.Errorf("Failed to find task ID %s. Considering the task terminated: %s\n", taskID, err.Error())
		return 0, false
	}

	if len(result.Items) == 0 {
		// That task is gone now (maybe someone else removed it). Our work here is done.
		return 0, true
	}

	exitCode := 1
	var done bool
	stopStates := []swarm.TaskState{
		swarm.TaskStateComplete,
		swarm.TaskStateFailed,
		swarm.TaskStateRejected,
	}

	for _, task := range result.Items {
		stop := false
		for _, stopState := range stopStates {
			if task.Status.State == stopState {
				stop = true
				break
			}
		}

		if stop {
			exitCode = task.Status.ContainerStatus.ExitCode

			if exitCode == 0 && task.Status.State == swarm.TaskStateRejected {
				exitCode = 255 // force non-zero exit for task rejected
			}
			done = true
			break
		}
	}
	return exitCode, done
}

func (j *RunServiceJob) deleteService(ctx *Context, runCtx context.Context, svcID string) error {
	if delete, _ := strconv.ParseBool(j.Delete); !delete {
		return nil
	}

	mc, err := j.getMobyClient()
	if err != nil {
		return fmt.Errorf("failed to get moby client for service removal: %w", err)
	}

	_, err = mc.ServiceRemove(runCtx, svcID, client.ServiceRemoveOptions{})

	// Service not found means it was already removed by another process; that's fine.
	if cerrdefs.IsNotFound(err) {
		ctx.Logger.Warningf("Service %s cannot be removed. An error may have happened, "+
			"or it might have been removed by another process", svcID)
		return nil
	}

	return err
}
