package instance

import (
	"context"
	"errors"
	"fmt"
	"github.com/cirruslabs/cirrus-ci-agent/api"
	"github.com/cirruslabs/cirrus-cli/internal/executor/heuristic"
	"github.com/cirruslabs/cirrus-cli/internal/executor/instance/abstract"
	"github.com/cirruslabs/cirrus-cli/internal/executor/instance/containerbackend"
	"github.com/cirruslabs/cirrus-cli/internal/executor/instance/persistentworker"
	"github.com/cirruslabs/cirrus-cli/internal/executor/instance/runconfig"
	"github.com/cirruslabs/cirrus-cli/internal/executor/options"
	"github.com/cirruslabs/cirrus-cli/internal/executor/platform"
	"github.com/cirruslabs/cirrus-cli/internal/logger"
	"github.com/cirruslabs/echelon"
	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/any"
	"math"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

var (
	ErrFailedToCreateInstance    = errors.New("failed to create instance")
	ErrUnsupportedInstance       = errors.New("unsupported instance type")
	ErrAdditionalContainerFailed = errors.New("additional container failed")
)

const (
	mebi = 1024 * 1024
	nano = 1_000_000_000
)

func NewFromProto(
	anyInstance *any.Any,
	commands []*api.Command,
	customWorkingDir string,
	logger logger.Lightweight,
) (abstract.Instance, error) {
	if anyInstance == nil {
		return nil, fmt.Errorf("%w: got nil instance which means it's probably not supported by the CLI",
			ErrFailedToCreateInstance)
	}

	var dynamicInstance ptypes.DynamicAny
	if err := ptypes.UnmarshalAny(anyInstance, &dynamicInstance); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal task's instance: %v",
			ErrFailedToCreateInstance, err)
	}

	switch instance := dynamicInstance.Message.(type) {
	case *api.ContainerInstance:
		var containerPlatform platform.Platform

		switch instance.Platform {
		case api.Platform_LINUX:
			containerPlatform = platform.NewUnix()
		case api.Platform_WINDOWS:
			containerPlatform = platform.NewWindows(instance.OsVersion)
		default:
			return nil, fmt.Errorf("%w: unsupported container instance platform: %s",
				ErrFailedToCreateInstance, instance.Platform.String())
		}

		return &ContainerInstance{
			Image:                instance.Image,
			CPU:                  instance.Cpu,
			Memory:               instance.Memory,
			AdditionalContainers: instance.AdditionalContainers,
			Platform:             containerPlatform,
			CustomWorkingDir:     customWorkingDir,
		}, nil
	case *api.PipeInstance:
		stages, err := PipeStagesFromCommands(commands)
		if err != nil {
			return nil, err
		}

		return &PipeInstance{
			CPU:              instance.Cpu,
			Memory:           instance.Memory,
			Stages:           stages,
			CustomWorkingDir: customWorkingDir,
		}, nil
	case *api.PrebuiltImageInstance:
		// PrebuiltImageInstance is currently missing the domain part to craft the full image name
		// used in the follow-up tasks.
		//
		// However, since currently the only possible value is "gcr.io",
		// we simply craft the image name manually using that hardcoded value.
		image := path.Join("gcr.io", instance.Repository) + ":" + instance.Reference

		return &PrebuiltInstance{
			Image:      image,
			Dockerfile: instance.Dockerfile,
			Arguments:  instance.Arguments,
		}, nil
	case *api.PersistentWorkerInstance:
		return persistentworker.New(instance.Isolation, logger)
	case *api.DockerBuilder:
		// Ensures that we're not trying to run e.g. Windows-specific scripts on macOS
		instanceOS := strings.ToLower(instance.Platform.String())
		if runtime.GOOS != instanceOS {
			return nil, fmt.Errorf("%w: cannot run %s Docker Builder instance on this platform",
				ErrFailedToCreateInstance, strings.Title(instanceOS))
		}

		return persistentworker.New(&api.Isolation{
			Type: &api.Isolation_None_{
				None: &api.Isolation_None{},
			},
		}, logger)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedInstance, instance)
	}
}

type Params struct {
	Image                  string
	CPU                    float32
	Memory                 uint32
	AdditionalContainers   []*api.AdditionalContainer
	CommandFrom, CommandTo string
	Platform               platform.Platform
	AgentVolumeName        string
	WorkingVolumeName      string
	WorkingDirectory       string
}

// nolint:gocognit
func RunContainerizedAgent(ctx context.Context, config *runconfig.RunConfig, params *Params) error {
	logger := config.Logger
	backend := config.ContainerBackend

	// Clamp resources to those available for container backend daemon
	info, err := backend.SystemInfo(ctx)
	if err != nil {
		return err
	}
	availableCPU := float32(info.TotalCPUs)
	availableMemory := uint32(info.TotalMemoryBytes / mebi)

	params.CPU = clampCPU(params.CPU, availableCPU)
	params.Memory = clampMemory(params.Memory, availableMemory)
	for _, additionalContainer := range params.AdditionalContainers {
		additionalContainer.Cpu = clampCPU(additionalContainer.Cpu, availableCPU)
		additionalContainer.Memory = clampMemory(additionalContainer.Memory, availableMemory)
	}

	if err := pullHelper(ctx, params.Image, backend, config.ContainerOptions, logger); err != nil {
		return err
	}

	logger.Debugf("creating container using working volume %s", params.WorkingVolumeName)
	input := containerbackend.ContainerCreateInput{
		Image: params.Image,
		Entrypoint: []string{
			params.Platform.ContainerAgentPath(),
			"-api-endpoint",
			config.ContainerEndpoint,
			"-server-token",
			config.ServerSecret,
			"-client-token",
			config.ClientSecret,
			"-task-id",
			strconv.FormatInt(config.TaskID, 10),
			"-command-from",
			params.CommandFrom,
			"-command-to",
			params.CommandTo,
		},
		Env: make(map[string]string),
		Mounts: []containerbackend.ContainerMount{
			{
				Type:   containerbackend.MountTypeVolume,
				Source: params.AgentVolumeName,
				Target: params.Platform.ContainerAgentVolumeDir(),
			},
		},
		Resources: containerbackend.ContainerResources{
			NanoCPUs: int64(params.CPU * nano),
			Memory:   int64(params.Memory * mebi),
		},
	}

	if runtime.GOOS == "linux" {
		if heuristic.GetCloudBuildIP(ctx) != "" {
			// Attach the container to the Cloud Build network for RPC the server
			// to be accessible in case we're running in Cloud Build and the CLI
			// itself is containerized (so we can't mount a Unix domain socket
			// because we don't know the path to it on the host)
			input.Network = heuristic.CloudBuildNetworkName
		}

		// Disable SELinux confinement for this container
		//
		// This solves the following problems when SELinux is enabled:
		// * agent not being able to connect to the CLI's Unix socket
		// * task container not being able to read project directory files when using dirty mode
		input.DisableSELinux = true
	}

	// Mount the  directory with the CLI's Unix domain socket in case it's used,
	// assuming that we run in the same mount namespace as the Docker daemon
	if strings.HasPrefix(config.ContainerEndpoint, "unix:") {
		socketPath := strings.TrimPrefix(config.ContainerEndpoint, "unix:")
		socketDir := filepath.Dir(socketPath)

		input.Mounts = append(input.Mounts, containerbackend.ContainerMount{
			Type:   containerbackend.MountTypeBind,
			Source: socketDir,
			Target: socketDir,
		})
	}

	if config.DirtyMode {
		// In dirty mode we mount the project directory from host
		input.Mounts = append(input.Mounts, containerbackend.ContainerMount{
			Type:   containerbackend.MountTypeBind,
			Source: config.ProjectDir,
			Target: params.WorkingDirectory,
		})
	} else {
		// Otherwise we mount the project directory's copy contained in a working volume
		input.Mounts = append(input.Mounts, containerbackend.ContainerMount{
			Type:   containerbackend.MountTypeVolume,
			Source: params.WorkingVolumeName,
			Target: params.WorkingDirectory,
		})
	}

	// In case the additional containers are used, tell the agent to wait for them
	if len(params.AdditionalContainers) > 0 {
		var ports []string
		for _, additionalContainer := range params.AdditionalContainers {
			for _, portMapping := range additionalContainer.Ports {
				ports = append(ports, strconv.FormatUint(uint64(portMapping.ContainerPort), 10))
			}
		}
		commaDelimitedPorts := strings.Join(ports, ",")
		input.Env["CIRRUS_PORTS_WAIT_FOR"] = commaDelimitedPorts
	}

	cont, err := backend.ContainerCreate(ctx, &input, "")
	if err != nil {
		return err
	}

	// Create controls for the additional containers
	//
	// We also separate the context here to gain a better control of the cancellation order:
	// when the parent context (ctx) is cancelled, the main container will be killed first,
	// and only then all the additional containers will be killed via a separate context
	// (additionalContainersCtx).
	var additionalContainersWG sync.WaitGroup
	additionalContainersCtx, additionalContainersCancel := context.WithCancel(context.Background())

	logReaderCtx, cancelLogReaderCtx := context.WithCancel(ctx)
	var logReaderWg sync.WaitGroup
	logReaderWg.Add(1)

	// Schedule all containers for removal
	defer func() {
		// We need to remove additional containers first in order to avoid Podman's
		// "has dependent containers which must be removed before it" error
		additionalContainersCancel()
		additionalContainersWG.Wait()

		if config.ContainerOptions.NoCleanup {
			logger.Infof("not cleaning up container %s, don't forget to remove it with \"docker rm -v %s\"",
				cont.ID, cont.ID)
		} else {
			logger.Debugf("cleaning up container %s", cont.ID)

			err := backend.ContainerDelete(context.Background(), cont.ID)
			if err != nil {
				logger.Warnf("error while removing container: %v", err)
			}
		}

		logger.Debugf("waiting for the container log reader to finish")
		cancelLogReaderCtx()
		logReaderWg.Wait()
	}()

	// Start additional containers (if any)
	additionalContainersErrChan := make(chan error, len(params.AdditionalContainers))
	for _, additionalContainer := range params.AdditionalContainers {
		additionalContainer := additionalContainer

		additionalContainersWG.Add(1)
		go func() {
			if err := runAdditionalContainer(
				additionalContainersCtx,
				logger,
				additionalContainer,
				backend,
				cont.ID,
				config.ContainerOptions,
			); err != nil {
				additionalContainersErrChan <- err
			}
			additionalContainersWG.Done()
		}()
	}

	logger.Debugf("starting container %s", cont.ID)
	if err := backend.ContainerStart(ctx, cont.ID); err != nil {
		return err
	}

	logChan, err := backend.ContainerLogs(logReaderCtx, cont.ID)
	if err != nil {
		return err
	}
	go func() {
		for logLine := range logChan {
			logger.Debugf("container: %s", logLine)
		}
		logReaderWg.Done()
	}()

	logger.Debugf("waiting for container %s to finish", cont.ID)
	waitChan, errChan := backend.ContainerWait(ctx, cont.ID)
	select {
	case res := <-waitChan:
		logger.Debugf("container exited with %v error and exit code %d", res.Error, res.StatusCode)
	case err := <-errChan:
		return err
	case acErr := <-additionalContainersErrChan:
		return acErr
	}

	return nil
}

func runAdditionalContainer(
	ctx context.Context,
	logger *echelon.Logger,
	additionalContainer *api.AdditionalContainer,
	backend containerbackend.ContainerBackend,
	connectToContainer string,
	containerOptions options.ContainerOptions,
) error {
	if err := pullHelper(ctx, additionalContainer.Image, backend, containerOptions, logger); err != nil {
		return err
	}

	logger.Debugf("creating additional container")
	input := &containerbackend.ContainerCreateInput{
		Image:   additionalContainer.Image,
		Command: additionalContainer.Command,
		Env:     additionalContainer.Environment,
		Resources: containerbackend.ContainerResources{
			NanoCPUs: int64(additionalContainer.Cpu * nano),
			Memory:   int64(additionalContainer.Memory * mebi),
		},
		Network: fmt.Sprintf("container:%s", connectToContainer),
	}
	cont, err := backend.ContainerCreate(ctx, input, "")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAdditionalContainerFailed, err)
	}

	defer func() {
		if containerOptions.NoCleanup {
			logger.Infof("not cleaning up additional container %s, don't forget to remove it with \"docker rm -v %s\"",
				cont.ID, cont.ID)

			return
		}

		logger.Debugf("cleaning up additional container %s", cont.ID)
		err := backend.ContainerDelete(context.Background(), cont.ID)
		if err != nil {
			logger.Warnf("Error while removing additional container: %v", err)
		}
	}()

	// We don't support port mappings at this moment: re-implementing them similarly to Kubernetes
	// would require fiddling with Netfilter, which results in unwanted complexity.
	//
	// So here we simply do our best effort and warn the user about potential problems.
	for _, portMapping := range additionalContainer.Ports {
		if portMapping.HostPort != 0 {
			logger.Warnf("port mappings are unsupported by the Cirrus CLI, please tell the application "+
				"running in the additional container '%s' to use a different port", additionalContainer.Name)
			break
		}
	}

	logger.Debugf("starting additional container %s", cont.ID)
	if err := backend.ContainerStart(ctx, cont.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrAdditionalContainerFailed, err)
	}

	logger.Debugf("waiting for additional container %s to finish", cont.ID)
	waitChan, errChan := backend.ContainerWait(ctx, cont.ID)
	select {
	case res := <-waitChan:
		logger.Debugf("additional container exited with %v error and exit code %d", res.Error, res.StatusCode)
	case err := <-errChan:
		return fmt.Errorf("%w: %v", ErrAdditionalContainerFailed, err)
	}

	return nil
}

func clampCPU(requested float32, available float32) float32 {
	return float32(math.Min(float64(requested), float64(available)))
}

func clampMemory(requested uint32, available uint32) uint32 {
	if requested > available {
		return available
	}

	return requested
}

func pullHelper(
	ctx context.Context,
	reference string,
	backend containerbackend.ContainerBackend,
	copts options.ContainerOptions,
	logger *echelon.Logger,
) error {
	if !copts.ShouldPullImage(ctx, backend, reference) {
		return nil
	}

	if logger == nil {
		logger = echelon.NewLogger(echelon.ErrorLevel, &RendererStub{})
	}

	dockerPullLogger := logger.Scoped("image pull")
	dockerPullLogger.Infof("Pulling image %s...", reference)

	if err := backend.ImagePull(ctx, reference); err != nil {
		dockerPullLogger.Errorf("Failed to pull %s: %v", reference, err)
		dockerPullLogger.Finish(false)

		return fmt.Errorf("%w: %v", ErrAdditionalContainerFailed, err)
	}

	dockerPullLogger.Finish(true)

	return nil
}
