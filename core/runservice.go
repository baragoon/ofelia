package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

const (
	// TODO are these const defined somewhere in the docker API?
	swarmError = -999
)

var svcChecker = time.NewTicker(watchDuration)

type RunServiceJob struct {
	BareJob `mapstructure:",squash"`
	Client  *docker.Client `json:"-"`
	mobyClient *client.Client `json:"-"` // Moby client for service operations
	User    string         `default:"root"`
	TTY     bool           `default:"false"`
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

// getMobyClient initializes or returns the cached moby client.
func (j *RunServiceJob) getMobyClient() (*client.Client, error) {
	if j.mobyClient != nil {
		return j.mobyClient, nil
	}
	// Create moby client from docker client endpoint
	endpoint := j.Client.Endpoint()
	opts := []client.Opt{
		client.WithHost(endpoint),
		client.WithAPIVersionNegotiation(),
	}
	c, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create moby client: %w", err)
	}
	j.mobyClient = c
	return c, nil
}

func (j *RunServiceJob) Run(ctx *Context) error {
	if err := j.pullImage(); err != nil {
		return err
	}

	svc, err := j.buildService(context.Background())

	if err != nil {
		return err
	}

	ctx.Logger.Noticef("Created service %s for job %s\n", svc.ID, j.Name)

	if err := j.watchContainer(ctx, svc.ID); err != nil {
		return err
	}

	return j.deleteService(ctx, svc.ID)
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
	// we need to attach it to the same network (if specified).
	// Note: Network configuration in moby ServiceSpec may need alternative handling
	
	if j.Command != "" {
		spec.TaskTemplate.ContainerSpec.Command = strings.Split(j.Command, " ")
	}

	opts := client.ServiceCreateOptions{
		Spec: spec,
	}

	result, err := mc.ServiceCreate(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Fetch the created service
	inspectResult, err := mc.ServiceInspect(ctx, result.ID, client.ServiceInspectOptions{})
	if err != nil {
		return nil, err
	}

	return &inspectResult.Service, nil
}

func (j *RunServiceJob) watchContainer(ctx *Context, svcID string) error {
	mc, err := j.getMobyClient()
	if err != nil {
		return fmt.Errorf("failed to get moby client: %w", err)
	}

	exitCode := swarmError

	ctx.Logger.Noticef("Checking for service ID %s (%s) termination\n", svcID, j.Name)

	inspectResult, err := mc.ServiceInspect(context.Background(), svcID, client.ServiceInspectOptions{})
	if err != nil {
		return fmt.Errorf("Failed to inspect service %s: %s", svcID, err.Error())
	}
	svc := &inspectResult.Service

	// On every tick, check if all the services have completed, or have error out
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		for range svcChecker.C {

			if svc.CreatedAt.After(time.Now().Add(maxProcessDuration)) {
				err = ErrMaxTimeRunning
				return
			}

			taskExitCode, found := j.findtaskstatus(ctx, svc.ID)

			if found {
				exitCode = taskExitCode
				return
			}
		}
	}()

	wg.Wait()

	ctx.Logger.Noticef("Service ID %s (%s) has completed with exit code %d\n", svcID, j.Name, exitCode)
	return err
}

func (j *RunServiceJob) findtaskstatus(ctx *Context, taskID string) (int, bool) {
	mc, err := j.getMobyClient()
	if err != nil {
		ctx.Logger.Errorf("Failed to get moby client for task lookup: %s\n", err.Error())
		return 0, false
	}

	filters := make(client.Filters)
	filters.Add("service", taskID)

	opts := client.TaskListOptions{
		Filters: filters,
	}

	result, err := mc.TaskList(context.Background(), opts)
	if err != nil {
		ctx.Logger.Errorf("Failed to find task ID %s. Considering the task terminated: %s\n", taskID, err.Error())
		return 0, false
	}

	if len(result.Items) == 0 {
		// That task is gone now (maybe someone else removed it. Our work here is done
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

func (j *RunServiceJob) deleteService(ctx *Context, svcID string) error {
	if delete, _ := strconv.ParseBool(j.Delete); !delete {
		return nil
	}

	mc, err := j.getMobyClient()
	if err != nil {
		return fmt.Errorf("failed to get moby client for service removal: %w", err)
	}

	_, err = mc.ServiceRemove(context.Background(), svcID, client.ServiceRemoveOptions{})

	// 404 or similar indicates service not found; that's okay
	if err != nil && strings.Contains(err.Error(), "not found") {
		ctx.Logger.Warningf("Service %s cannot be removed. An error may have happened, "+
			"or it might have been removed by another process", svcID)
		return nil
	}

	return err

}
