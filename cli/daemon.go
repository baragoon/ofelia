package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/baragoon/ofelia/core"
)

const (
	defaultWebUIBindAddress = ":8080"
	defaultWebUIRefresh     = 10
)

// UIServer abstracts the web UI lifecycle so the cli package does not need to
// import the webui package (which itself imports cli).
type UIServer interface {
	Start() error
	Stop(ctx context.Context) error
}

// DaemonCommand daemon process
type DaemonCommand struct {
	ConfigFile        string   `long:"config" description:"configuration file" default:"/etc/ofelia.conf"`
	DockerLabelConfig bool     `short:"d" long:"docker" description:"continiously poll docker labels for configurations"`
	DockerFilters     []string `short:"f" long:"docker-filter" description:"filter to select docker containers. https://docs.docker.com/reference/cli/docker/container/ls/#filter"`
	WebUI             bool     `long:"ui" description:"enable built-in web UI for hosts and jobs"`
	WebUIBind         string   `long:"ui-bind" description:"web UI bind address" default:":8080"`
	WebUIRefreshSec   int      `long:"ui-refresh-sec" description:"web UI auto-refresh interval in seconds" default:"10"`
	DockerHosts       []string `long:"docker-host" description:"docker host endpoint. Can be provided multiple times to schedule docker jobs on all hosts (e.g. tcp://host:2376)"`
	DockerTLSVerify   bool     `long:"docker-tls-verify" description:"enable TLS verification for --docker-host connections"`
	DockerCertPath    string   `long:"docker-cert-path" description:"path to docker TLS certs directory (contains ca.pem, cert.pem, key.pem)"`
	DockerCACert      string   `long:"docker-ca-cert" description:"path to CA certificate PEM file for docker TLS"`
	DockerCert        string   `long:"docker-cert" description:"path to client certificate PEM file for docker TLS"`
	DockerKey         string   `long:"docker-key" description:"path to client key PEM file for docker TLS"`
	// UIServerFactory is called during boot to create the web UI server.
	// Injected by the caller to avoid a circular import with the webui package.
	UIServerFactory   func(bind string, refresh int, config *Config, logger core.Logger) UIServer
	scheduler         *core.Scheduler
	uiServer          UIServer
	signals           chan os.Signal
	done              chan bool
	Logger            core.Logger
}

// Execute runs the daemon
func (c *DaemonCommand) Execute(args []string) error {
	if err := c.boot(); err != nil {
		return err
	}

	if err := c.start(); err != nil {
		return err
	}

	if err := c.shutdown(); err != nil {
		return err
	}

	return nil
}

func (c *DaemonCommand) boot() (err error) {
	c.applyWebUIEnvOverrides()

	// Always try to read the config file, as there are options such as globals or some tasks that can be specified there and not in docker
	config, err := BuildFromFile(c.ConfigFile, c.Logger)
	if err != nil {
		if !c.DockerLabelConfig {
			return fmt.Errorf("can't read the config file: %w", err)
		} else {
			c.Logger.Debugf("Config file %v not found. Proceeding to read docker labels...", c.ConfigFile)
		}
	} else {
		msg := "Found config file %v"
		if c.DockerLabelConfig {
			msg += ". Proceeding to read docker labels as well..."
		}
		c.Logger.Debugf(msg, c.ConfigFile)
	}

	scheduler := core.NewScheduler(c.Logger)

	config.sh = scheduler
	config.buildSchedulerMiddlewares(scheduler)

	config.dockerHandler, err = NewDockerHandler(config, c.DockerFilters, c.DockerLabelConfig, c.Logger, DockerConnectionOptions{
		Hosts:      c.DockerHosts,
		TLSVerify:  c.DockerTLSVerify,
		CertPath:   c.DockerCertPath,
		CACertFile: c.DockerCACert,
		CertFile:   c.DockerCert,
		KeyFile:    c.DockerKey,
	})
	if err != nil {
		return fmt.Errorf("failed to create docker handler: %w", err)
	}

	err = config.InitializeApp()
	if err != nil {
		return fmt.Errorf("can't start the app: %w", err)
	}

	c.scheduler = config.sh

	if c.WebUI && c.UIServerFactory != nil {
		c.uiServer = c.UIServerFactory(c.WebUIBind, c.WebUIRefreshSec, config, c.Logger)
	}

	return err
}

func (c *DaemonCommand) start() error {
	c.setSignals()
	if err := c.scheduler.Start(); err != nil {
		return err
	}

	if c.uiServer != nil {
		if err := c.uiServer.Start(); err != nil {
			_ = c.scheduler.Stop()
			return err
		}
	}

	return nil
}

func (c *DaemonCommand) setSignals() {
	c.signals = make(chan os.Signal, 1)
	c.done = make(chan bool, 1)

	signal.Notify(c.signals, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-c.signals
		c.Logger.Warningf(
			"Signal received: %s, shutting down the process\n", sig,
		)

		c.done <- true
	}()
}

func (c *DaemonCommand) shutdown() error {
	<-c.done

	if c.uiServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.uiServer.Stop(ctx); err != nil {
			c.Logger.Warningf("failed to stop web UI server: %v", err)
		}
	}

	if !c.scheduler.IsRunning() {
		return nil
	}

	c.Logger.Warningf("Waiting running jobs.")
	return c.scheduler.Stop()
}

func (c *DaemonCommand) applyWebUIEnvOverrides() {
	if !c.WebUI {
		if parsed, ok := parseEnvBool("OFELIA_UI"); ok {
			c.WebUI = parsed
		}
	}

	if c.WebUIBind == defaultWebUIBindAddress {
		if v := strings.TrimSpace(os.Getenv("OFELIA_UI_BIND")); v != "" {
			c.WebUIBind = normalizeWebUIBind(v)
		}
	}

	if c.WebUIRefreshSec == defaultWebUIRefresh {
		if v := strings.TrimSpace(os.Getenv("OFELIA_UI_REFRESH_SEC")); v != "" {
			refresh, convErr := strconv.Atoi(v)
			if convErr != nil {
				if c.Logger != nil {
					c.Logger.Warningf("Ignoring invalid OFELIA_UI_REFRESH_SEC value %q: %v", v, convErr)
				}
				return
			}
			c.WebUIRefreshSec = refresh
		}
	}
}

func parseEnvBool(name string) (bool, bool) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false, false
	}

	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, false
	}

	return parsed, true
}

func normalizeWebUIBind(bind string) string {
	bind = strings.TrimSpace(bind)
	if bind == "" {
		return bind
	}

	isNumericPort := true
	for _, r := range bind {
		if r < '0' || r > '9' {
			isNumericPort = false
			break
		}
	}

	if isNumericPort {
		return ":" + bind
	}

	return bind
}
