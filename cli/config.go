package cli

import (
	"fmt"
	"strings"
	"sync"

	"github.com/baragoon/ofelia/core"
	"github.com/baragoon/ofelia/middlewares"
	docker "github.com/fsouza/go-dockerclient"

	defaults "github.com/mcuadros/go-defaults"
	gcfg "gopkg.in/gcfg.v1"
)

const (
	jobExec       = "job-exec"
	jobRun        = "job-run"
	jobServiceRun = "job-service-run"
	jobLocal      = "job-local"
)

// Config contains the configuration
type Config struct {
	Global struct {
		middlewares.SlackConfig `mapstructure:",squash"`
		middlewares.SaveConfig  `mapstructure:",squash"`
		middlewares.MailConfig  `mapstructure:",squash"`
	}
	ExecJobs    map[string]*ExecJobConfig    `gcfg:"job-exec" mapstructure:"job-exec,squash"`
	RunJobs     map[string]*RunJobConfig     `gcfg:"job-run" mapstructure:"job-run,squash"`
	ServiceJobs map[string]*RunServiceConfig `gcfg:"job-service-run" mapstructure:"job-service-run,squash"`
	LocalJobs   map[string]*LocalJobConfig   `gcfg:"job-local" mapstructure:"job-local,squash"`

	sh            *core.Scheduler
	dockerHandler *DockerHandler
	logger        core.Logger
	mu            sync.RWMutex
}

func NewConfig(logger core.Logger) *Config {
	// Initialize
	c := &Config{}
	c.ExecJobs = make(map[string]*ExecJobConfig)
	c.RunJobs = make(map[string]*RunJobConfig)
	c.ServiceJobs = make(map[string]*RunServiceConfig)
	c.LocalJobs = make(map[string]*LocalJobConfig)
	c.logger = logger
	defaults.SetDefaults(c)
	return c
}

// BuildFromFile builds a scheduler using the config from a file
func BuildFromFile(filename string, logger core.Logger) (*Config, error) {
	c := NewConfig(logger)
	err := gcfg.ReadFileInto(c, filename)
	return c, err
}

// BuildFromString builds a scheduler using the config from a string
func BuildFromString(config string, logger core.Logger) (*Config, error) {
	c := NewConfig(logger)
	if err := gcfg.ReadStringInto(c, config); err != nil {
		return nil, err
	}
	return c, nil
}

// Call this only once at app init
func (c *Config) InitializeApp() error {
	if c.sh == nil {
		return fmt.Errorf("scheduler is not initialized yet")
	}

	if c.dockerHandler.ConfigFromLabelsEnabled() {
		// Wait couple seconds for docker to propagate container labels
		c.dockerHandler.WaitForLabels()

		// In order to support non dynamic job types such as Local or Run using labels
		// lets parse the labels and merge the job lists
		dockerLabels, err := c.dockerHandler.GetDockerLabels()
		if err != nil {
			return err
		}

		if err := c.buildFromDockerLabels(dockerLabels); err != nil {
			return err
		}

		// Initialize middlewares again after reading the labels
		c.buildSchedulerMiddlewares(c.sh)
	}

	c.fanOutDockerJobs()

	c.mu.Lock()
	defer c.mu.Unlock()

	for name, j := range c.ExecJobs {
		defaults.SetDefaults(j)
		j.Client = c.resolveDockerClient(j.DockerHost)
		j.Name = name
		j.buildMiddlewares()
		c.sh.AddJob(j)
	}

	for name, j := range c.RunJobs {
		defaults.SetDefaults(j)
		j.Client = c.resolveDockerClient(j.DockerHost)
		j.Name = name
		j.buildMiddlewares()
		c.sh.AddJob(j)
	}

	for name, j := range c.LocalJobs {
		defaults.SetDefaults(j)
		j.Name = name
		j.buildMiddlewares()
		c.sh.AddJob(j)
	}

	for name, j := range c.ServiceJobs {
		defaults.SetDefaults(j)
		j.Name = name
		j.Client = c.resolveDockerClient(j.DockerHost)
		j.buildMiddlewares()
		c.sh.AddJob(j)
	}

	return nil
}

func (c *Config) resolveDockerClient(host string) *docker.Client {
	if c.dockerHandler == nil {
		return nil
	}

	if host == "" {
		return c.dockerHandler.GetInternalDockerClient()
	}

	if client, ok := c.dockerHandler.GetInternalDockerClients()[host]; ok {
		return client
	}

	return c.dockerHandler.GetInternalDockerClient()
}

func (c *Config) fanOutDockerJobs() {
	if c.dockerHandler == nil {
		return
	}

	clients := c.dockerHandler.GetInternalDockerClients()
	if len(clients) <= 1 {
		for _, j := range c.ExecJobs {
			if j.DockerHost == "" {
				j.DockerHost = c.dockerHandler.GetPrimaryDockerHost()
			}
		}
		for _, j := range c.RunJobs {
			if j.DockerHost == "" {
				j.DockerHost = c.dockerHandler.GetPrimaryDockerHost()
			}
		}
		for _, j := range c.ServiceJobs {
			if j.DockerHost == "" {
				j.DockerHost = c.dockerHandler.GetPrimaryDockerHost()
			}
		}
		return
	}

	c.ExecJobs = fanOutExecJobs(c.ExecJobs, clients)
	c.RunJobs = fanOutRunJobs(c.RunJobs, clients)
	c.ServiceJobs = fanOutServiceJobs(c.ServiceJobs, clients)
}

func fanOutExecJobs(src map[string]*ExecJobConfig, clients map[string]*docker.Client) map[string]*ExecJobConfig {
	dst := make(map[string]*ExecJobConfig)
	for name, j := range src {
		if j.DockerHost != "" || strings.Contains(name, "::") {
			dst[name] = j
			continue
		}

		for host := range clients {
			dst[fmt.Sprintf("%s::%s", host, name)] = cloneExecJobConfigForHost(j, host)
		}
	}

	return dst
}

func fanOutRunJobs(src map[string]*RunJobConfig, clients map[string]*docker.Client) map[string]*RunJobConfig {
	dst := make(map[string]*RunJobConfig)
	for name, j := range src {
		if j.DockerHost != "" || strings.Contains(name, "::") {
			dst[name] = j
			continue
		}

		for host := range clients {
			dst[fmt.Sprintf("%s::%s", host, name)] = cloneRunJobConfigForHost(j, host)
		}
	}

	return dst
}

func fanOutServiceJobs(src map[string]*RunServiceConfig, clients map[string]*docker.Client) map[string]*RunServiceConfig {
	dst := make(map[string]*RunServiceConfig)
	for name, j := range src {
		if j.DockerHost != "" || strings.Contains(name, "::") {
			dst[name] = j
			continue
		}

		for host := range clients {
			dst[fmt.Sprintf("%s::%s", host, name)] = cloneRunServiceJobConfigForHost(j, host)
		}
	}

	return dst
}

func cloneExecJobConfigForHost(src *ExecJobConfig, host string) *ExecJobConfig {
	return &ExecJobConfig{
		ExecJob: core.ExecJob{
			BareJob: core.BareJob{
				Schedule: src.Schedule,
				Command:  src.Command,
			},
			Container:   src.Container,
			User:        src.User,
			TTY:         src.TTY,
			Environment: append([]string(nil), src.Environment...),
		},
		DockerHost:    host,
		OverlapConfig: src.OverlapConfig,
		SlackConfig:   src.SlackConfig,
		SaveConfig:    src.SaveConfig,
		MailConfig:    src.MailConfig,
	}
}

func cloneRunJobConfigForHost(src *RunJobConfig, host string) *RunJobConfig {
	return &RunJobConfig{
		RunJob: core.RunJob{
			BareJob: core.BareJob{
				Schedule: src.Schedule,
				Command:  src.Command,
			},
			User:        src.User,
			TTY:         src.TTY,
			Delete:      src.Delete,
			Pull:        src.Pull,
			Image:       src.Image,
			Network:     src.Network,
			Hostname:    src.Hostname,
			Container:   src.Container,
			Volume:      append([]string(nil), src.Volume...),
			VolumesFrom: append([]string(nil), src.VolumesFrom...),
			Environment: append([]string(nil), src.Environment...),
		},
		DockerHost:    host,
		OverlapConfig: src.OverlapConfig,
		SlackConfig:   src.SlackConfig,
		SaveConfig:    src.SaveConfig,
		MailConfig:    src.MailConfig,
	}
}

func cloneRunServiceJobConfigForHost(src *RunServiceConfig, host string) *RunServiceConfig {
	return &RunServiceConfig{
		RunServiceJob: core.RunServiceJob{
			BareJob: core.BareJob{
				Schedule: src.Schedule,
				Command:  src.Command,
			},
			User:    src.User,
			TTY:     src.TTY,
			Delete:  src.Delete,
			Image:   src.Image,
			Network: src.Network,
		},
		DockerHost:    host,
		OverlapConfig: src.OverlapConfig,
		SlackConfig:   src.SlackConfig,
		SaveConfig:    src.SaveConfig,
		MailConfig:    src.MailConfig,
	}
}

func (c *Config) JobsCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.ExecJobs) + len(c.RunJobs) + len(c.LocalJobs) + len(c.ServiceJobs)
}

func (c *Config) buildSchedulerMiddlewares(sh *core.Scheduler) {
	sh.Use(middlewares.NewSlack(&c.Global.SlackConfig))
	sh.Use(middlewares.NewSave(&c.Global.SaveConfig))
	sh.Use(middlewares.NewMail(&c.Global.MailConfig))
}

func (c *Config) dockerLabelsUpdate(labels map[string]map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Get the current labels
	var parsedLabelConfig Config
	parsedLabelConfig.buildFromDockerLabels(labels)

	// Calculate the delta execJobs
	for name, j := range c.ExecJobs {
		found := false
		for newJobsName, newJob := range parsedLabelConfig.ExecJobs {
			// Check if the schedule has changed
			if name == newJobsName {
				found = true
				// There is a slight race condition were a job can be canceled / restarted with different params
				// so, lets take care of it by simply restarting
				// For the hash to work properly, we must fill the fields before calling it
				defaults.SetDefaults(newJob)
				newJob.Client = c.resolveDockerClient(newJob.DockerHost)
				newJob.Name = newJobsName
				if newJob.Hash() != j.Hash() {
					c.logger.Debugf("Job %s has changed, restarting", name)
					// Remove from the scheduler
					c.sh.RemoveJob(j)
					// Add the job back to the scheduler
					newJob.buildMiddlewares()
					c.sh.AddJob(newJob)
					// Update the job config
					c.ExecJobs[name] = newJob
				}
				break
			}
		}
		if !found {
			c.logger.Debugf("Job %s is not found, Removing", name)
			// Remove the job
			c.sh.RemoveJob(j)
			delete(c.ExecJobs, name)
		}
	}

	// Check for aditions
	for newJobsName, newJob := range parsedLabelConfig.ExecJobs {
		found := false
		for name := range c.ExecJobs {
			if name == newJobsName {
				found = true
				break
			}
		}
		if !found {
			defaults.SetDefaults(newJob)
			newJob.Client = c.resolveDockerClient(newJob.DockerHost)
			newJob.Name = newJobsName
			newJob.buildMiddlewares()
			c.sh.AddJob(newJob)
			c.ExecJobs[newJobsName] = newJob
		}
	}

	for name, j := range c.RunJobs {
		found := false
		for newJobsName, newJob := range parsedLabelConfig.RunJobs {
			// Check if the schedule has changed
			if name == newJobsName {
				found = true
				// There is a slight race condition were a job can be canceled / restarted with different params
				// so, lets take care of it by simply restarting
				// For the hash to work properly, we must fill the fields before calling it
				defaults.SetDefaults(newJob)
				newJob.Client = c.resolveDockerClient(newJob.DockerHost)
				newJob.Name = newJobsName
				if newJob.Hash() != j.Hash() {
					// Remove from the scheduler
					c.sh.RemoveJob(j)
					// Add the job back to the scheduler
					newJob.buildMiddlewares()
					c.sh.AddJob(newJob)
					// Update the job config
					c.RunJobs[name] = newJob
				}
				break
			}
		}
		if !found {
			// Remove the job
			c.sh.RemoveJob(j)
			delete(c.RunJobs, name)
		}
	}

	// Check for aditions
	for newJobsName, newJob := range parsedLabelConfig.RunJobs {
		found := false
		for name := range c.RunJobs {
			if name == newJobsName {
				found = true
				break
			}
		}
		if !found {
			defaults.SetDefaults(newJob)
			newJob.Client = c.resolveDockerClient(newJob.DockerHost)
			newJob.Name = newJobsName
			newJob.buildMiddlewares()
			c.sh.AddJob(newJob)
			c.RunJobs[newJobsName] = newJob
		}
	}
}

// ExecJobConfig contains all configuration params needed to build a ExecJob
type ExecJobConfig struct {
	core.ExecJob              `mapstructure:",squash"`
	DockerHost                string `gcfg:"docker-host" mapstructure:"docker-host"`
	middlewares.OverlapConfig `mapstructure:",squash"`
	middlewares.SlackConfig   `mapstructure:",squash"`
	middlewares.SaveConfig    `mapstructure:",squash"`
	middlewares.MailConfig    `mapstructure:",squash"`
}

func (c *ExecJobConfig) buildMiddlewares() {
	c.ExecJob.Use(middlewares.NewOverlap(&c.OverlapConfig))
	c.ExecJob.Use(middlewares.NewSlack(&c.SlackConfig))
	c.ExecJob.Use(middlewares.NewSave(&c.SaveConfig))
	c.ExecJob.Use(middlewares.NewMail(&c.MailConfig))
}

// RunServiceConfig contains all configuration params needed to build a RunJob
type RunServiceConfig struct {
	core.RunServiceJob        `mapstructure:",squash"`
	DockerHost                string `gcfg:"docker-host" mapstructure:"docker-host"`
	middlewares.OverlapConfig `mapstructure:",squash"`
	middlewares.SlackConfig   `mapstructure:",squash"`
	middlewares.SaveConfig    `mapstructure:",squash"`
	middlewares.MailConfig    `mapstructure:",squash"`
}

type RunJobConfig struct {
	core.RunJob               `mapstructure:",squash"`
	DockerHost                string `gcfg:"docker-host" mapstructure:"docker-host"`
	middlewares.OverlapConfig `mapstructure:",squash"`
	middlewares.SlackConfig   `mapstructure:",squash"`
	middlewares.SaveConfig    `mapstructure:",squash"`
	middlewares.MailConfig    `mapstructure:",squash"`
}

func (c *RunJobConfig) buildMiddlewares() {
	c.RunJob.Use(middlewares.NewOverlap(&c.OverlapConfig))
	c.RunJob.Use(middlewares.NewSlack(&c.SlackConfig))
	c.RunJob.Use(middlewares.NewSave(&c.SaveConfig))
	c.RunJob.Use(middlewares.NewMail(&c.MailConfig))
}

// LocalJobConfig contains all configuration params needed to build a RunJob
type LocalJobConfig struct {
	core.LocalJob             `mapstructure:",squash"`
	middlewares.OverlapConfig `mapstructure:",squash"`
	middlewares.SlackConfig   `mapstructure:",squash"`
	middlewares.SaveConfig    `mapstructure:",squash"`
	middlewares.MailConfig    `mapstructure:",squash"`
}

func (c *LocalJobConfig) buildMiddlewares() {
	c.LocalJob.Use(middlewares.NewOverlap(&c.OverlapConfig))
	c.LocalJob.Use(middlewares.NewSlack(&c.SlackConfig))
	c.LocalJob.Use(middlewares.NewSave(&c.SaveConfig))
	c.LocalJob.Use(middlewares.NewMail(&c.MailConfig))
}

func (c *RunServiceConfig) buildMiddlewares() {
	c.RunServiceJob.Use(middlewares.NewOverlap(&c.OverlapConfig))
	c.RunServiceJob.Use(middlewares.NewSlack(&c.SlackConfig))
	c.RunServiceJob.Use(middlewares.NewSave(&c.SaveConfig))
	c.RunServiceJob.Use(middlewares.NewMail(&c.MailConfig))
}
