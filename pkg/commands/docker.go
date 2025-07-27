package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	ogLog "log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	cliconfig "github.com/docker/cli/cli/config"
	ddocker "github.com/docker/cli/cli/context/docker"
	ctxstore "github.com/docker/cli/cli/context/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/imdario/mergo"
	"github.com/jesseduffield/lazydocker/pkg/commands/ssh"
	"github.com/jesseduffield/lazydocker/pkg/config"
	"github.com/jesseduffield/lazydocker/pkg/i18n"
	"github.com/jesseduffield/lazydocker/pkg/utils"
	"github.com/sasha-s/go-deadlock"
	"github.com/sirupsen/logrus"
)

const (
	APIVersion       = "1.25"
	dockerHostEnvKey = "DOCKER_HOST"
)

// DockerCommand is our main docker interface
type DockerCommand struct {
	Log                    *logrus.Entry
	OSCommand              *OSCommand
	Tr                     *i18n.TranslationSet
	Config                 *config.AppConfig
	Client                 *client.Client
	InDockerComposeProject bool
	ErrorChan              chan error
	ContainerMutex         deadlock.Mutex
	ServiceMutex           deadlock.Mutex

	Closers []io.Closer
}

var _ io.Closer = &DockerCommand{}

// LimitedDockerCommand is a stripped-down DockerCommand with just the methods the container/service/image might need
type LimitedDockerCommand interface {
	NewCommandObject(CommandObject) CommandObject
}

// CommandObject is what we pass to our template resolvers when we are running a custom command. We do not guarantee that all fields will be populated: just the ones that make sense for the current context
type CommandObject struct {
	DockerCompose string
	Service       *Service
	Container     *Container
	Image         *Image
	Volume        *Volume
	Network       *Network
}

// NewCommandObject takes a command object and returns a default command object with the passed command object merged in
func (c *DockerCommand) NewCommandObject(obj CommandObject) CommandObject {
	defaultObj := CommandObject{DockerCompose: c.Config.UserConfig.CommandTemplates.DockerCompose}
	_ = mergo.Merge(&defaultObj, obj)
	return defaultObj
}

// NewDockerCommand it runs docker commands
func NewDockerCommand(log *logrus.Entry, osCommand *OSCommand, tr *i18n.TranslationSet, config *config.AppConfig, errorChan chan error) (*DockerCommand, error) {
	var engineHost string
	var err error

	// Determine which container engine to use
	containerEngine := config.UserConfig.ContainerEngine
	log.Info("Container engine from config: " + containerEngine)
	
	// If container engine is not explicitly set, detect it
	if containerEngine == "" {
		containerEngine = DetectContainerEngine(osCommand)
		log.Info("Auto-detected container engine: " + containerEngine)
		config.UserConfig.ContainerEngine = containerEngine
	} else {
		log.Info("Using container engine from config: " + containerEngine)
	}
	
	// Determine appropriate host based on container engine
	if containerEngine == "podman" {
		// On macOS, check if the podman machine is running
		if runtime.GOOS == "darwin" {
			// First check if podman is installed
			isPodmanInstalled := osCommand.RunCommand("which podman") == nil
			if !isPodmanInstalled {
				log.Warn("Podman doesn't appear to be installed. Please install Podman first.")
				fmt.Println("Podman not found. Install with: brew install podman")
			} else {
				// Check if podman machine is running
				machineStatus, err := osCommand.RunCommandWithOutput("podman machine list --format json")
				if err == nil && strings.Contains(machineStatus, "\"Running\":true") {
					fmt.Println("Podman machine is running")
					
					// Set up a TCP connection for the Podman API (this is more reliable on macOS)
					// First, check if podman machine is responsive
					cmd := exec.Command("podman", "machine", "inspect")
					_, err = cmd.CombinedOutput()
					
					// Try to set up port forwarding for the Podman API
					// This works for some Podman versions, may not work for all
					portCmd := exec.Command("podman", "machine", "ssh", "sudo", "systemctl", "restart", "podman.socket")
					_, err = portCmd.CombinedOutput()
					if err != nil {
						fmt.Println("Could not restart podman.socket in the VM. This is normal for some Podman versions.")
					}
					
					// Try to set up direct access via podman-mac-helper (if available)
					helperCmd := exec.Command("which", "podman-mac-helper")
					helperOutput, helperErr := helperCmd.CombinedOutput()
					if helperErr == nil && len(helperOutput) > 0 {
						fmt.Println("Found podman-mac-helper, setting up direct connection")
						// Get the path to the socket from the helper
						socketCmd := exec.Command("podman-mac-helper", "socket")
						socketOutput, socketErr := socketCmd.CombinedOutput()
						if socketErr == nil && len(socketOutput) > 0 {
							socketPath := strings.TrimSpace(string(socketOutput))
							fmt.Printf("Using socket path from podman-mac-helper: %s\n", socketPath)
							os.Setenv("DOCKER_HOST", "unix://"+socketPath)
						}
					} else {
						// Try to directly connect to the VM's Docker API
						fmt.Println("Using direct connection to Podman machine")
						
						// Try to get socket path directly from machine inspect
						socketCmd := exec.Command("podman", "machine", "inspect", "--format", "{{.ConnectionInfo.PodmanSocket.Path}}")
						socketOutput, socketErr := socketCmd.CombinedOutput()
						if socketErr == nil && len(socketOutput) > 0 {
							socketPath := strings.TrimSpace(string(socketOutput))
							if socketPath != "" {
								fmt.Printf("Found Podman socket via machine inspect: %s\n", socketPath)
								os.Setenv("DOCKER_HOST", "unix://" + socketPath)
							}
						}
						
						// For newer versions of Podman on macOS, use TCP connection to VM's IP
						// First, get the VM's IP address
						ipCmd := exec.Command("podman", "machine", "inspect", "--format", "{{.Host.CurrentIP}}")
						ipOutput, ipErr := ipCmd.CombinedOutput()
						if ipErr == nil && len(ipOutput) > 0 {
							vmIP := strings.TrimSpace(string(ipOutput))
							if vmIP != "" {
								fmt.Printf("Found Podman VM IP: %s\n", vmIP)
								// Use the Docker API port (2375/2376)
								os.Setenv("DOCKER_HOST", fmt.Sprintf("tcp://%s:2375", vmIP))
							} else {
								// Fallback to localhost with port mapping
								fmt.Println("Could not determine VM IP, using localhost:8080")
								os.Setenv("DOCKER_HOST", "tcp://localhost:8080")
							}
						} else {
							// Fallback to default Docker socket (just in case there's a compatibility layer)
							fmt.Println("Using default Docker socket as fallback")
							os.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
						}
					}
				} else {
					log.Warn("Podman machine doesn't appear to be running. Start it with: podman machine start")
					fmt.Println("Podman machine not running. Try: podman machine start")
				}
			}
		} else {
			// On Linux, check if podman socket service is running
			systemctlErr := osCommand.RunCommand("systemctl --user is-active --quiet podman.socket")
			if systemctlErr != nil {
				log.Warn("Podman socket service doesn't appear to be running. You may need to run: systemctl --user start podman.socket")
				fmt.Println("Podman socket service not running. Try: systemctl --user start podman.socket")
			} else {
				fmt.Println("Podman socket service is running")
			}
		}
		
		engineHost = GetDefaultPodmanHost()
		log.Info("Using Podman container engine with host: " + engineHost)
		fmt.Println("Using Podman container engine with host: " + engineHost)
	} else {
		// Default to Docker
		engineHost, err = determineDockerHost()
		if err != nil {
			ogLog.Printf("> could not determine docker host %v", err)
		}
		log.Info("Using Docker container engine with host: " + engineHost)
		fmt.Println("Using Docker container engine with host: " + engineHost)
	}

	// NOTE: Inject the determined host to the environment. This allows the
	//       `SSHHandler.HandleSSHDockerHost()` to create a local unix socket tunneled
	//       over SSH to the specified ssh host.
	if strings.HasPrefix(engineHost, "ssh://") {
		os.Setenv(dockerHostEnvKey, engineHost)
	}

	tunnelCloser, err := ssh.NewSSHHandler(osCommand).HandleSSHDockerHost()
	if err != nil {
		ogLog.Fatal(err)
	}

	// Retrieve the docker host from the environment which could have been set by
	// the `SSHHandler.HandleSSHDockerHost()` and override `engineHost`.
	dockerHostFromEnv := os.Getenv(dockerHostEnvKey)
	if dockerHostFromEnv != "" {
		engineHost = dockerHostFromEnv
	}

	// Create client using the appropriate host (Docker or Podman socket)
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion(APIVersion), client.WithHost(engineHost))
	if err != nil {
		ogLog.Fatal(err)
	}

	dockerCommand := &DockerCommand{
		Log:                    log,
		OSCommand:              osCommand,
		Tr:                     tr,
		Config:                 config,
		Client:                 cli,
		ErrorChan:              errorChan,
		InDockerComposeProject: true,
		Closers:                []io.Closer{tunnelCloser},
	}

	dockerCommand.setDockerComposeCommand(config)

	err = osCommand.RunCommand(
		utils.ApplyTemplate(
			config.UserConfig.CommandTemplates.CheckDockerComposeConfig,
			dockerCommand.NewCommandObject(CommandObject{}),
		),
	)
	if err != nil {
		dockerCommand.InDockerComposeProject = false
		log.Warn(err.Error())
	}

	return dockerCommand, nil
}

func (c *DockerCommand) setDockerComposeCommand(config *config.AppConfig) {
	containerEngine := config.UserConfig.ContainerEngine
	
	// Set default based on the selected container engine
	if containerEngine == "podman" {
		// If using Podman, use "podman-compose" by default
		if config.UserConfig.CommandTemplates.DockerCompose == "docker compose" {
			// Check if podman-compose is available
			err := c.OSCommand.RunCommand("podman-compose --version")
			if err == nil {
				config.UserConfig.CommandTemplates.DockerCompose = "podman-compose"
				c.Log.Info("Using podman-compose as compose command")
			} else {
				// Fallback to regular podman compose if supported
				err := c.OSCommand.RunCommand("podman compose --version")
				if err == nil {
					config.UserConfig.CommandTemplates.DockerCompose = "podman compose"
					c.Log.Info("Using podman compose as compose command")
				} else {
					// If neither is available, warn and fall back to docker compose
					c.Log.Warn("podman-compose not found, falling back to docker compose. Install podman-compose for better integration.")
				}
			}
		}
	} else {
		// Docker engine (default)
		if config.UserConfig.CommandTemplates.DockerCompose != "docker compose" {
			return
		}

		// it's possible that a user is still using docker-compose, so we'll check if 'docker compose' is available, and if not, we'll fall back to 'docker-compose'
		err := c.OSCommand.RunCommand("docker compose version")
		if err != nil {
			config.UserConfig.CommandTemplates.DockerCompose = "docker-compose"
			c.Log.Info("Using docker-compose as compose command")
		}
	}
}

func (c *DockerCommand) Close() error {
	return utils.CloseMany(c.Closers)
}

func (c *DockerCommand) CreateClientStatMonitor(container *Container) {
	container.MonitoringStats = true
	stream, err := c.Client.ContainerStats(context.Background(), container.ID, true)
	if err != nil {
		// not creating error panel because if we've disconnected from docker we'll
		// have already created an error panel
		c.Log.Error(err)
		container.MonitoringStats = false
		return
	}

	defer stream.Body.Close()

	scanner := bufio.NewScanner(stream.Body)
	for scanner.Scan() {
		data := scanner.Bytes()
		var stats ContainerStats
		_ = json.Unmarshal(data, &stats)

		recordedStats := &RecordedStats{
			ClientStats: stats,
			DerivedStats: DerivedStats{
				CPUPercentage:    stats.CalculateContainerCPUPercentage(),
				MemoryPercentage: stats.CalculateContainerMemoryUsage(),
			},
			RecordedAt: time.Now(),
		}

		container.appendStats(recordedStats, c.Config.UserConfig.Stats.MaxDuration)
	}

	container.MonitoringStats = false
}

func (c *DockerCommand) RefreshContainersAndServices(currentServices []*Service, currentContainers []*Container) ([]*Container, []*Service, error) {
	c.ServiceMutex.Lock()
	defer c.ServiceMutex.Unlock()

	containers, err := c.GetContainers(currentContainers)
	if err != nil {
		return nil, nil, err
	}

	var services []*Service
	// we only need to get these services once because they won't change in the runtime of the program
	if currentServices != nil {
		services = currentServices
	} else {
		services, err = c.GetServices()
		if err != nil {
			return nil, nil, err
		}
	}

	c.assignContainersToServices(containers, services)

	return containers, services, nil
}

func (c *DockerCommand) assignContainersToServices(containers []*Container, services []*Service) {
L:
	for _, service := range services {
		for _, ctr := range containers {
			if !ctr.OneOff && ctr.ServiceName == service.Name {
				service.Container = ctr
				continue L
			}
		}
		service.Container = nil
	}
}

// GetContainers gets the docker containers
func (c *DockerCommand) GetContainers(existingContainers []*Container) ([]*Container, error) {
	c.ContainerMutex.Lock()
	defer c.ContainerMutex.Unlock()

	containers, err := c.Client.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}

	ownContainers := make([]*Container, len(containers))

	for i, ctr := range containers {
		var newContainer *Container

		// check if we already have data stored against the container
		for _, existingContainer := range existingContainers {
			if existingContainer.ID == ctr.ID {
				newContainer = existingContainer
				break
			}
		}

		// initialise the container if it's completely new
		if newContainer == nil {
			newContainer = &Container{
				ID:            ctr.ID,
				Client:        c.Client,
				OSCommand:     c.OSCommand,
				Log:           c.Log,
				DockerCommand: c,
				Tr:            c.Tr,
			}
		}

		newContainer.Container = ctr
		// if the container is made with a name label we will use that
		if name, ok := ctr.Labels["name"]; ok {
			newContainer.Name = name
		} else {
			newContainer.Name = strings.TrimLeft(ctr.Names[0], "/")
		}
		newContainer.ServiceName = ctr.Labels["com.docker.compose.service"]
		newContainer.ProjectName = ctr.Labels["com.docker.compose.project"]
		newContainer.ContainerNumber = ctr.Labels["com.docker.compose.container"]
		newContainer.OneOff = ctr.Labels["com.docker.compose.oneoff"] == "True"

		ownContainers[i] = newContainer
	}

	c.SetContainerDetails(ownContainers)

	return ownContainers, nil
}

// GetServices gets services
func (c *DockerCommand) GetServices() ([]*Service, error) {
	if !c.InDockerComposeProject {
		return nil, nil
	}

	composeCommand := c.Config.UserConfig.CommandTemplates.DockerCompose
	output, err := c.OSCommand.RunCommandWithOutput(fmt.Sprintf("%s config --services", composeCommand))
	if err != nil {
		return nil, err
	}

	// output looks like:
	// service1
	// service2

	lines := utils.SplitLines(output)
	services := make([]*Service, len(lines))
	for i, str := range lines {
		services[i] = &Service{
			Name:          str,
			ID:            str,
			OSCommand:     c.OSCommand,
			Log:           c.Log,
			DockerCommand: c,
		}
	}

	return services, nil
}

func (c *DockerCommand) RefreshContainerDetails(containers []*Container) error {
	c.ContainerMutex.Lock()
	defer c.ContainerMutex.Unlock()

	c.SetContainerDetails(containers)

	return nil
}

// Attaches the details returned from docker inspect to each of the containers
// this contains a bit more info than what you get from the go-docker client
func (c *DockerCommand) SetContainerDetails(containers []*Container) {
	wg := sync.WaitGroup{}
	for _, ctr := range containers {
		ctr := ctr
		wg.Add(1)
		go func() {
			details, err := c.Client.ContainerInspect(context.Background(), ctr.ID)
			if err != nil {
				c.Log.Error(err)
			} else {
				ctr.Details = details
			}
			wg.Done()
		}()
	}
	wg.Wait()
}

// ViewAllLogs attaches to a subprocess viewing all the logs from docker-compose
func (c *DockerCommand) ViewAllLogs() (*exec.Cmd, error) {
	cmd := c.OSCommand.ExecutableFromString(
		utils.ApplyTemplate(
			c.OSCommand.Config.UserConfig.CommandTemplates.ViewAllLogs,
			c.NewCommandObject(CommandObject{}),
		),
	)

	c.OSCommand.PrepareForChildren(cmd)

	return cmd, nil
}

// DockerComposeConfig returns the result of 'docker-compose config'
func (c *DockerCommand) DockerComposeConfig() string {
	output, err := c.OSCommand.RunCommandWithOutput(
		utils.ApplyTemplate(
			c.OSCommand.Config.UserConfig.CommandTemplates.DockerComposeConfig,
			c.NewCommandObject(CommandObject{}),
		),
	)
	if err != nil {
		output = err.Error()
	}
	return output
}

// DetectContainerEngine checks if Docker or Podman is available and returns the detected engine
// Returns "docker" or "podman" based on what's available, defaults to "docker" if both are available
func DetectContainerEngine(osCommand *OSCommand) string {
	// Check if Podman is available
	isPodmanAvailable := osCommand.RunCommand("podman version") == nil
	fmt.Printf("Podman available: %v\n", isPodmanAvailable)

	// Check if Docker is available
	isDockerAvailable := osCommand.RunCommand("docker version") == nil
	fmt.Printf("Docker available: %v\n", isDockerAvailable)

	// For macOS, check if podman machine is running 
	if runtime.GOOS == "darwin" && isPodmanAvailable {
		machineStatus, err := osCommand.RunCommandWithOutput("podman machine list --format json")
		if err == nil && strings.Contains(machineStatus, "\"Running\":true") {
			fmt.Println("Auto-detecting Podman on macOS with running machine")
			return "podman"
		}
	}
	
	// If Podman is available but Docker is not, use Podman
	if isPodmanAvailable && !isDockerAvailable {
		fmt.Println("Auto-detecting Podman as container engine")
		return "podman"
	}
	
	// Default to Docker
	fmt.Println("Auto-detecting Docker as container engine")
	return "docker"
}

// determineDockerHost tries to the determine the docker host that we should connect to
// in the following order of decreasing precedence:
//   - value of "DOCKER_HOST" environment variable
//   - host retrieved from the current context (specified via DOCKER_CONTEXT)
//   - "default docker host" for the host operating system, otherwise
func determineDockerHost() (string, error) {
	// If the docker host is explicitly set via the "DOCKER_HOST" environment variable,
	// then its a no-brainer :shrug:
	if os.Getenv("DOCKER_HOST") != "" {
		return os.Getenv("DOCKER_HOST"), nil
	}

	currentContext := os.Getenv("DOCKER_CONTEXT")
	if currentContext == "" {
		cf, err := cliconfig.Load(cliconfig.Dir())
		if err != nil {
			return "", err
		}
		currentContext = cf.CurrentContext
	}

	// On some systems (windows) `default` is stored in the docker config as the currentContext.
	if currentContext == "" || currentContext == "default" {
		// If a docker context is neither specified via the "DOCKER_CONTEXT" environment variable nor via the
		// $HOME/.docker/config file, then we fall back to connecting to the "default docker host" meant for
		// the host operating system.
		return defaultDockerHost, nil
	}

	storeConfig := ctxstore.NewConfig(
		func() interface{} { return &ddocker.EndpointMeta{} },
		ctxstore.EndpointTypeGetter(ddocker.DockerEndpoint, func() interface{} { return &ddocker.EndpointMeta{} }),
	)

	st := ctxstore.New(cliconfig.ContextStoreDir(), storeConfig)
	md, err := st.GetMetadata(currentContext)
	if err != nil {
		return "", err
	}
	dockerEP, ok := md.Endpoints[ddocker.DockerEndpoint]
	if !ok {
		return "", err
	}
	dockerEPMeta, ok := dockerEP.(ddocker.EndpointMeta)
	if !ok {
		return "", fmt.Errorf("expected docker.EndpointMeta, got %T", dockerEP)
	}

	if dockerEPMeta.Host != "" {
		return dockerEPMeta.Host, nil
	}

	// We might end up here, if the context was created with the `host` set to an empty value (i.e. '').
	// For example:
	// ```sh
	// docker context create foo --docker "host="
	// ```
	// In such scenario, we mimic the `docker` cli and try to connect to the "default docker host".
	return defaultDockerHost, nil
}
