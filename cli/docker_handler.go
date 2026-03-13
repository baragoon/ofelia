package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/baragoon/ofelia/core"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/go-viper/mapstructure/v2"
)

const (
	labelPrefix = "ofelia"

	requiredLabel       = labelPrefix + ".enabled"
	requiredLabelFilter = requiredLabel + "=true"
	serviceLabel        = labelPrefix + ".service"
)

var (
	errNoContainersMatchingFilters = errors.New("no containers matching filters")
	errInvalidDockerFilter         = errors.New("invalid docker filter")
	errFailedToListContainers      = errors.New("failed to list containers")
)

const (
	dockerHostIndexedEnvPrefix = "DOCKER_HOST_"
	dockerHostDottedEnvPrefix  = "DOCKER_HOST."
)

type DockerHandler struct {
	dockerClients     map[string]*docker.Client
	primaryDockerHost string
	notifier          labelConfigUpdater
	configsFromLabels bool
	logger            core.Logger
	filters           []string
	connOpts          DockerConnectionOptions
}

type DockerConnectionOptions struct {
	Hosts      []string
	TLSVerify  bool
	CertPath   string
	CACertFile string
	CertFile   string
	KeyFile    string
}

type labelConfigUpdater interface {
	dockerLabelsUpdate(map[string]map[string]string)
}

// TODO: Implement an interface so the code does not have to use third parties directly
func (c *DockerHandler) GetInternalDockerClient() *docker.Client {
	if len(c.dockerClients) == 0 {
		return nil
	}

	if d, ok := c.dockerClients[c.primaryDockerHost]; ok {
		return d
	}

	for _, d := range c.dockerClients {
		return d
	}

	return nil
}

func (c *DockerHandler) GetInternalDockerClients() map[string]*docker.Client {
	return c.dockerClients
}

func (c *DockerHandler) GetPrimaryDockerHost() string {
	return c.primaryDockerHost
}

func (c *DockerHandler) buildDockerClient(host string) (*docker.Client, string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		d, err := docker.NewClientFromEnv()
		if err != nil {
			return nil, "", err
		}

		resolvedHost := os.Getenv("DOCKER_HOST")
		if resolvedHost == "" {
			resolvedHost = "default"
		}

		return d, resolvedHost, nil
	}

	ca, cert, key := c.resolveTLSFiles()
	tlsVerify := c.connOpts.TLSVerify || os.Getenv("DOCKER_TLS_VERIFY") != ""
	if tlsVerify {
		if ca == "" || cert == "" || key == "" {
			return nil, "", fmt.Errorf("docker TLS is enabled for host %q but certificate files are incomplete", host)
		}

		d, err := docker.NewTLSClient(host, cert, key, ca)
		if err != nil {
			return nil, "", err
		}

		return d, host, nil
	}

	d, err := docker.NewClient(host)
	if err != nil {
		return nil, "", err
	}

	return d, host, nil
}

func (c *DockerHandler) resolveTLSFiles() (ca, cert, key string) {
	certPath := c.connOpts.CertPath
	if certPath == "" {
		certPath = os.Getenv("DOCKER_CERT_PATH")
	}

	ca = strings.TrimSpace(c.connOpts.CACertFile)
	cert = strings.TrimSpace(c.connOpts.CertFile)
	key = strings.TrimSpace(c.connOpts.KeyFile)

	if certPath != "" {
		if ca == "" {
			ca = filepath.Join(certPath, "ca.pem")
		}
		if cert == "" {
			cert = filepath.Join(certPath, "cert.pem")
		}
		if key == "" {
			key = filepath.Join(certPath, "key.pem")
		}
	}

	return ca, cert, key
}

func NewDockerHandler(config *Config, dockerFilters []string, configsFromLabels bool, logger core.Logger, connOpts DockerConnectionOptions) (*DockerHandler, error) {
	if len(dockerFilters) > 0 && !configsFromLabels {
		return nil, fmt.Errorf("docker filters can only be provided together with '--docker' flag")
	}

	c := &DockerHandler{
		filters:           dockerFilters,
		configsFromLabels: configsFromLabels,
		notifier:          config,
		logger:            logger,
		connOpts:          connOpts,
		dockerClients:     make(map[string]*docker.Client),
	}

	hosts := resolveConfiguredDockerHosts(connOpts.Hosts)
	if len(hosts) == 0 {
		hosts = []string{""}
	}

	for _, host := range hosts {
		d, resolvedHost, err := c.buildDockerClient(host)
		if err != nil {
			c.logger.Warningf("Skipping Docker host %q: failed to build client: %v", strings.TrimSpace(host), err)
			continue
		}
		// Do a sanity check on docker.
		if _, err := d.Info(); err != nil {
			c.logger.Warningf("Skipping Docker host %q: failed health check: %v", resolvedHost, err)
			continue
		}

		hostKey := normalizeDockerHostKey(resolvedHost)
		c.dockerClients[hostKey] = d
		if c.primaryDockerHost == "" {
			c.primaryDockerHost = hostKey
		}
	}

	if len(c.dockerClients) == 0 {
		return nil, fmt.Errorf("no reachable docker hosts from %d configured host(s)", len(hosts))
	}

	if c.configsFromLabels {
		go c.watch()
	}
	return c, nil
}

func resolveConfiguredDockerHosts(cliHosts []string) []string {
	if len(cliHosts) > 0 {
		return cliHosts
	}

	if hosts := parseDockerHostsFromPatternedEnv(); len(hosts) > 0 {
		return hosts
	}

	// Keep backwards compatibility for the single-host DOCKER_HOST behavior.
	dockerHost := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if hasDockerHostsListSeparator(dockerHost) {
		return parseDockerHosts(dockerHost)
	}

	return nil
}

type dockerHostEnvEntry struct {
	key   string
	sort  string
	value string
	index int
	named bool
}

func parseDockerHostsFromPatternedEnv() []string {
	entries := make([]dockerHostEnvEntry, 0)
	for _, rawEnv := range os.Environ() {
		key, value, ok := strings.Cut(rawEnv, "=")
		if !ok {
			continue
		}

		suffix, matched := dockerHostEnvSuffix(key)
		if !matched {
			continue
		}

		entry := dockerHostEnvEntry{
			key:   key,
			sort:  suffix,
			value: strings.TrimSpace(value),
			index: -1,
			named: true,
		}

		if index, ok := parseDockerHostEnvIndex(suffix); ok {
			entry.index = index
			entry.named = false
		}

		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return nil
	}

	sort.Slice(entries, func(i, j int) bool {
		left := entries[i]
		right := entries[j]

		if left.named != right.named {
			return !left.named
		}
		if !left.named && left.index != right.index {
			return left.index < right.index
		}
		if left.sort != right.sort {
			return left.sort < right.sort
		}

		return left.key < right.key
	})

	hosts := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.value == "" {
			continue
		}
		if _, ok := seen[entry.value]; ok {
			continue
		}
		seen[entry.value] = struct{}{}
		hosts = append(hosts, entry.value)
	}

	if len(hosts) == 0 {
		return nil
	}

	return hosts
}

func dockerHostEnvSuffix(key string) (string, bool) {
	if strings.HasPrefix(key, dockerHostIndexedEnvPrefix) {
		suffix := strings.TrimSpace(strings.TrimPrefix(key, dockerHostIndexedEnvPrefix))
		return suffix, suffix != ""
	}

	if strings.HasPrefix(key, dockerHostDottedEnvPrefix) {
		suffix := strings.TrimSpace(strings.TrimPrefix(key, dockerHostDottedEnvPrefix))
		return suffix, suffix != ""
	}

	return "", false
}

func parseDockerHostEnvIndex(suffix string) (int, bool) {
	suffix = strings.TrimSpace(suffix)
	if strings.HasPrefix(suffix, "[") && strings.HasSuffix(suffix, "]") {
		suffix = strings.TrimSuffix(strings.TrimPrefix(suffix, "["), "]")
	}

	index, err := strconv.Atoi(suffix)
	if err != nil || index < 0 {
		return 0, false
	}

	return index, true
}

func hasDockerHostsListSeparator(hosts string) bool {
	return strings.ContainsAny(hosts, ",; \n\t ")
}

func parseDockerHosts(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\t', ' ':
			return true
		default:
			return false
		}
	})

	hosts := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		host := strings.TrimSpace(part)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}

	if len(hosts) == 0 {
		return nil
	}

	return hosts
}

func (c *DockerHandler) ConfigFromLabelsEnabled() bool {
	return c.configsFromLabels
}

func (c *DockerHandler) watch() {
	const pollInterval = 10 * time.Second
	c.logger.Debugf("Watching for Docker labels changes every %s...", pollInterval)
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for range ticker.C {
		labels, err := c.GetDockerLabels()
		// Do not print or care if there is no container up right now
		if err != nil && !errors.Is(err, errNoContainersMatchingFilters) {
			c.logger.Debugf("%v", err)
		}
		c.notifier.dockerLabelsUpdate(labels)
	}
}

func (c *DockerHandler) WaitForLabels() {
	const maxRetries = 3
	const retryDelay = 1 * time.Second
	const dockerEnvFile = "/.dockerenv"
	const mountinfoFilePath = "/proc/self/mountinfo"

	// Check if .dockerenv file exists
	if _, err := os.Stat(dockerEnvFile); os.IsNotExist(err) {
		c.logger.Debugf(".dockerenv file not found, ofelia is not running in a Docker container")
		return
	}

	id, err := getContainerID(mountinfoFilePath)
	if err != nil {
		c.logger.Debugf("Failed to extract ofelia's container ID. Trying with container hostname instead...")
		id, _ = os.Hostname()
	}

	for attempt := 0; attempt < maxRetries; attempt++ {
		primaryClient := c.GetInternalDockerClient()
		if primaryClient == nil {
			return
		}
		_, err := primaryClient.InspectContainerWithOptions(docker.InspectContainerOptions{ID: id})
		if err == nil {
			c.logger.Debugf("Found ofelia container with ID: %s", id)
			return
		}

		time.Sleep(retryDelay)
	}
}

func (c *DockerHandler) GetDockerLabels() (map[string]map[string]string, error) {
	var filters = map[string][]string{
		"label": {requiredLabelFilter},
	}
	for _, f := range c.filters {
		key, value, err := parseFilter(f)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, f)
		}
		filters[key] = append(filters[key], value)
	}

	var labels = make(map[string]map[string]string)
	missingHosts := 0
	for host, dc := range c.dockerClients {
		conts, err := dc.ListContainers(docker.ListContainersOptions{Filters: filters})
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errFailedToListContainers, err)
		}

		if len(conts) == 0 {
			missingHosts++
			continue
		}

		for _, cont := range conts {
			if len(cont.Names) == 0 || len(cont.Labels) == 0 {
				continue
			}

			name := strings.TrimPrefix(cont.Names[0], "/")
			filtered := make(map[string]string)
			for k, v := range cont.Labels {
				if strings.HasPrefix(k, labelPrefix) {
					filtered[k] = v
				}
			}

			if len(filtered) == 0 {
				continue
			}

			labels[makeContainerRef(host, name)] = filtered
		}
	}

	if missingHosts == len(c.dockerClients) {
		return nil, fmt.Errorf("%w: %v", errNoContainersMatchingFilters, filters)
	}

	return labels, nil
}

func normalizeDockerHostKey(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return "default"
	}

	host = strings.TrimPrefix(host, "tcp://")
	host = strings.TrimPrefix(host, "unix://")
	host = strings.TrimPrefix(host, "npipe://")
	host = strings.TrimSuffix(host, "/")
	host = strings.ReplaceAll(host, "/", "_")
	host = strings.ReplaceAll(host, ":", "_")
	if host == "" {
		return "default"
	}

	return host
}

func makeContainerRef(host, container string) string {
	return fmt.Sprintf("%s|%s", host, container)
}

func splitContainerRef(ref string) (host, container string) {
	parts := strings.SplitN(ref, "|", 2)
	if len(parts) != 2 {
		return "", ref
	}

	return parts[0], parts[1]
}

func parseFilter(filter string) (key, value string, err error) {
	parts := strings.SplitN(filter, "=", 2)
	if len(parts) != 2 {
		return "", "", errInvalidDockerFilter
	}
	return parts[0], parts[1], nil
}

func (c *Config) buildFromDockerLabels(labels map[string]map[string]string) error {
	execJobs := make(map[string]map[string]interface{})
	localJobs := make(map[string]map[string]interface{})
	runJobs := make(map[string]map[string]interface{})
	serviceJobs := make(map[string]map[string]interface{})
	globalConfigs := make(map[string]interface{})

	for containerRef, l := range labels {
		host, containerName := splitContainerRef(containerRef)
		isServiceContainer := func() bool {
			for k, v := range l {
				if k == serviceLabel {
					return v == "true"
				}
			}
			return false
		}()

		for k, v := range l {
			parts := strings.Split(k, ".")
			if len(parts) < 4 {
				if isServiceContainer {
					globalConfigs[parts[1]] = v
				}

				continue
			}

			jobType, jobName, jopParam := parts[1], parts[2], parts[3]
			hostJobName := jobName
			if host != "" {
				hostJobName = fmt.Sprintf("%s::%s", host, jobName)
				if jobType == jobExec && !isServiceContainer {
					hostJobName = fmt.Sprintf("%s:%s:%s", host, containerName, jobName)
				}
			}
			switch {
			case jobType == jobExec: // only job exec can be provided on the non-service container
				if _, ok := execJobs[hostJobName]; !ok {
					execJobs[hostJobName] = make(map[string]interface{})
				}

				setJobParam(execJobs[hostJobName], jopParam, v)
				if host != "" {
					execJobs[hostJobName]["docker-host"] = host
				}
				// since this label was placed not on the service container
				// this means we need to `exec` command in this container
				if !isServiceContainer {
					execJobs[hostJobName]["container"] = containerName
				}
			case jobType == jobLocal && isServiceContainer:
				if _, ok := localJobs[hostJobName]; !ok {
					localJobs[hostJobName] = make(map[string]interface{})
				}
				setJobParam(localJobs[hostJobName], jopParam, v)
			case jobType == jobServiceRun && isServiceContainer:
				if _, ok := serviceJobs[hostJobName]; !ok {
					serviceJobs[hostJobName] = make(map[string]interface{})
				}
				setJobParam(serviceJobs[hostJobName], jopParam, v)
				if host != "" {
					serviceJobs[hostJobName]["docker-host"] = host
				}
			case jobType == jobRun && isServiceContainer:
				if _, ok := runJobs[hostJobName]; !ok {
					runJobs[hostJobName] = make(map[string]interface{})
				}
				setJobParam(runJobs[hostJobName], jopParam, v)
				if host != "" {
					runJobs[hostJobName]["docker-host"] = host
				}
			default:
				// TODO: warn about unknown parameter
			}
		}
	}

	if len(globalConfigs) > 0 {
		if err := mapstructure.WeakDecode(globalConfigs, &c.Global); err != nil {
			return err
		}
	}

	if len(execJobs) > 0 {
		if err := mapstructure.WeakDecode(execJobs, &c.ExecJobs); err != nil {
			return err
		}
	}

	if len(localJobs) > 0 {
		if err := mapstructure.WeakDecode(localJobs, &c.LocalJobs); err != nil {
			return err
		}
	}

	if len(serviceJobs) > 0 {
		if err := mapstructure.WeakDecode(serviceJobs, &c.ServiceJobs); err != nil {
			return err
		}
	}

	if len(runJobs) > 0 {
		if err := mapstructure.WeakDecode(runJobs, &c.RunJobs); err != nil {
			return err
		}
	}

	return nil
}

func setJobParam(params map[string]interface{}, paramName, paramVal string) {
	switch strings.ToLower(paramName) {
	case "volume", "environment", "volumes-from":
		arr := []string{} // allow providing JSON arr of volume mounts
		if err := json.Unmarshal([]byte(paramVal), &arr); err == nil {
			params[paramName] = arr
			return
		}
	}

	params[paramName] = paramVal
}

func getContainerID(mountinfoFilePath string) (string, error) {
	// Open the mountinfo file
	file, err := os.Open(mountinfoFilePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// Scan the file line by line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		// Look for container ID in the line
		if !strings.Contains(line, "/containers/") {
			continue
		}

		splt := strings.Split(line, "/")
		for i, part := range splt {
			if part == "containers" && len(splt) > i+1 {
				return splt[i+1], nil
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", os.ErrNotExist
}
