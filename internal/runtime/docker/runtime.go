package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/uptimenine/serve/internal/runtime"
)

type Runtime struct {
	client       *client.Client
	registryAuth registryAuthProvider
}

func NewFromEnv() (*Runtime, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return New(cli), nil
}

func New(cli *client.Client) *Runtime {
	return &Runtime{client: cli, registryAuth: newDockerConfigAuthProvider()}
}

func (r *Runtime) PullImage(ctx context.Context, imageRef string) error {
	registryAuth, err := r.registryAuth.RegistryAuth(ctx, imageRef)
	if err != nil {
		return err
	}
	reader, err := r.client.ImagePull(ctx, imageRef, image.PullOptions{RegistryAuth: registryAuth})
	if err != nil {
		return err
	}
	defer reader.Close()
	return jsonmessage.DisplayJSONMessagesStream(reader, io.Discard, 0, false, nil)
}

func (r *Runtime) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	env, err := readEnvFiles(spec.EnvFiles)
	if err != nil {
		return "", err
	}
	env = append(envMapEntries(spec.Env), env...)
	config := &container.Config{
		Image:        spec.Image,
		Cmd:          spec.Command,
		Labels:       copyStringMap(spec.Labels),
		Env:          env,
		ExposedPorts: exposedPorts(spec.Ports),
	}
	hostConfig := &container.HostConfig{
		Binds:        append([]string(nil), spec.Volumes...),
		PortBindings: portBindings(spec.Ports),
		RestartPolicy: container.RestartPolicy{
			Name:              container.RestartPolicyMode(spec.Restart.Policy),
			MaximumRetryCount: spec.Restart.MaxAttempts,
		},
	}
	networkingConfig := &network.NetworkingConfig{}
	if spec.Network != "" {
		networkingConfig.EndpointsConfig = map[string]*network.EndpointSettings{
			spec.Network: {Aliases: append([]string(nil), spec.Aliases...)},
		}
	}

	response, err := r.client.ContainerCreate(ctx, config, hostConfig, networkingConfig, nil, spec.Name)
	if err != nil {
		return "", err
	}
	return runtime.ContainerID(response.ID), nil
}

func (r *Runtime) StartContainer(ctx context.Context, id runtime.ContainerID) error {
	return r.client.ContainerStart(ctx, string(id), container.StartOptions{})
}

func (r *Runtime) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	seconds := int(timeout.Seconds())
	if timeout > 0 && seconds == 0 {
		seconds = 1
	}
	err := r.client.ContainerStop(ctx, string(id), container.StopOptions{Timeout: &seconds})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *Runtime) RemoveContainer(ctx context.Context, id runtime.ContainerID) error {
	err := r.client.ContainerRemove(ctx, string(id), container.RemoveOptions{Force: true})
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *Runtime) InspectContainer(ctx context.Context, id runtime.ContainerID) (runtime.ContainerState, error) {
	inspect, err := r.client.ContainerInspect(ctx, string(id))
	if err != nil {
		return runtime.ContainerState{}, err
	}
	return inspectState(inspect), nil
}

func (r *Runtime) ListContainers(ctx context.Context, containerFilters runtime.ContainerFilters) ([]runtime.ContainerState, error) {
	args := filters.NewArgs()
	for key, value := range containerFilters.Labels {
		if value == "" {
			args.Add("label", key)
			continue
		}
		args.Add("label", key+"="+value)
	}
	containers, err := r.client.ContainerList(ctx, container.ListOptions{All: true, Filters: args})
	if err != nil {
		return nil, err
	}

	states := make([]runtime.ContainerState, 0, len(containers))
	for _, listed := range containers {
		inspect, err := r.client.ContainerInspect(ctx, listed.ID)
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		states = append(states, inspectState(inspect))
	}
	return states, nil
}

func (r *Runtime) Logs(ctx context.Context, id runtime.ContainerID, opts runtime.LogOptions) (io.ReadCloser, error) {
	_ = opts
	logs, err := r.client.ContainerLogs(ctx, string(id), container.LogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return nil, err
	}

	reader, writer := io.Pipe()
	go func() {
		_, copyErr := stdcopy.StdCopy(writer, writer, logs)
		closeErr := logs.Close()
		if copyErr != nil {
			_ = writer.CloseWithError(copyErr)
			return
		}
		_ = writer.CloseWithError(closeErr)
	}()
	return reader, nil
}

func (r *Runtime) Events(ctx context.Context) (<-chan runtime.RuntimeEvent, error) {
	args := filters.NewArgs()
	args.Add("type", string(events.ContainerEventType))
	dockerEvents, dockerErrors := r.client.Events(ctx, events.ListOptions{Filters: args})
	out := make(chan runtime.RuntimeEvent, 32)

	go func() {
		defer close(out)
		for {
			select {
			case event, ok := <-dockerEvents:
				if !ok {
					return
				}
				mapped, ok := mapEvent(event)
				if !ok {
					continue
				}
				select {
				case out <- mapped:
				case <-ctx.Done():
					return
				}
			case err, ok := <-dockerErrors:
				if !ok || err == nil {
					continue
				}
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return out, nil
}

func (r *Runtime) ExecContainer(ctx context.Context, id runtime.ContainerID, cmd []string) (string, error) {
	exec, err := r.client.ContainerExecCreate(ctx, string(id), container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return "", err
	}

	attach, err := r.client.ContainerExecAttach(ctx, exec.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer attach.Close()

	var output strings.Builder
	if _, err := stdcopy.StdCopy(&output, &output, attach.Reader); err != nil {
		return output.String(), err
	}

	inspect, err := r.client.ContainerExecInspect(ctx, exec.ID)
	if err != nil {
		return output.String(), err
	}
	if inspect.ExitCode != 0 {
		return output.String(), fmt.Errorf("exec %q exited with code %d: %s", strings.Join(cmd, " "), inspect.ExitCode, strings.TrimSpace(output.String()))
	}
	return output.String(), nil
}

func (r *Runtime) CreateNetwork(ctx context.Context, spec runtime.NetworkSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("network name is required")
	}
	_, err := r.client.NetworkCreate(ctx, spec.Name, network.CreateOptions{Driver: "bridge"})
	if errdefs.IsConflict(err) {
		return nil
	}
	return err
}

func (r *Runtime) RemoveNetwork(ctx context.Context, name string) error {
	err := r.client.NetworkRemove(ctx, name)
	if errdefs.IsNotFound(err) {
		return nil
	}
	return err
}

func (r *Runtime) ReplaceNetworkAliases(ctx context.Context, id runtime.ContainerID, networkName string, aliases []string) error {
	inspect, err := r.client.ContainerInspect(ctx, string(id))
	if err != nil {
		return err
	}
	if inspect.NetworkSettings == nil {
		return fmt.Errorf("container %s has no network attachments", inspect.Name)
	}
	endpoint, attached := inspect.NetworkSettings.Networks[networkName]
	if !attached || endpoint == nil {
		return fmt.Errorf("container %s is not attached to network %s", strings.TrimPrefix(inspect.Name, "/"), networkName)
	}

	name := strings.TrimPrefix(inspect.Name, "/")
	current := customNetworkAliases(endpoint.Aliases, inspect.ID, name)
	if sameAliasSet(current, aliases) {
		return nil
	}
	if err := r.client.NetworkDisconnect(ctx, networkName, string(id), true); err != nil {
		return fmt.Errorf("disconnect container %s from network %s: %w", name, networkName, err)
	}
	if err := r.client.NetworkConnect(ctx, networkName, string(id), &network.EndpointSettings{Aliases: append([]string(nil), aliases...)}); err != nil {
		restoreErr := r.client.NetworkConnect(ctx, networkName, string(id), &network.EndpointSettings{Aliases: current})
		return errors.Join(
			fmt.Errorf("connect container %s to network %s with replacement aliases: %w", name, networkName, err),
			wrapRestoreAliasesError(name, networkName, restoreErr),
		)
	}
	return nil
}

func wrapRestoreAliasesError(name string, networkName string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("restore aliases for container %s on network %s: %w", name, networkName, err)
}

func envMapEntries(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}

func readEnvFiles(paths []string) ([]string, error) {
	var env []string
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read env file %q: %w", path, err)
		}
		scanner := bufio.NewScanner(file)
		lineNumber := 0
		for scanner.Scan() {
			lineNumber++
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !strings.Contains(line, "=") {
				file.Close()
				return nil, fmt.Errorf("invalid env file %q line %d: expected KEY=value", path, lineNumber)
			}
			env = append(env, line)
		}
		if err := scanner.Err(); err != nil {
			file.Close()
			return nil, fmt.Errorf("scan env file %q: %w", path, err)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close env file %q: %w", path, err)
		}
	}
	return env, nil
}

func portBindings(ports []runtime.Port) nat.PortMap {
	bindings := nat.PortMap{}
	for _, port := range ports {
		if port.ContainerPort <= 0 || port.HostPort <= 0 {
			continue
		}
		hostIP := port.HostIP
		if hostIP == "" {
			hostIP = "127.0.0.1"
		}
		containerPort := nat.Port(fmt.Sprintf("%d/tcp", port.ContainerPort))
		bindings[containerPort] = append(bindings[containerPort], nat.PortBinding{
			HostIP:   hostIP,
			HostPort: strconv.Itoa(port.HostPort),
		})
	}
	if len(bindings) == 0 {
		return nil
	}
	return bindings
}

func exposedPorts(ports []runtime.Port) nat.PortSet {
	if len(ports) == 0 {
		return nil
	}
	set := nat.PortSet{}
	for _, port := range ports {
		if port.ContainerPort <= 0 {
			continue
		}
		set[nat.Port(fmt.Sprintf("%d/tcp", port.ContainerPort))] = struct{}{}
	}
	return set
}

func inspectState(inspect dockertypes.ContainerJSON) runtime.ContainerState {
	state := runtime.ContainerState{
		ID:     runtime.ContainerID(inspect.ID),
		Name:   strings.TrimPrefix(inspect.Name, "/"),
		Labels: mapFromConfig(inspect.Config),
		Health: runtime.HealthUnknown,
	}
	if inspect.Config != nil {
		state.Image = inspect.Config.Image
		state.Command = append([]string(nil), inspect.Config.Cmd...)
	}
	if inspect.HostConfig != nil {
		state.Restart = runtime.RestartPolicy{
			Policy:      string(inspect.HostConfig.RestartPolicy.Name),
			MaxAttempts: inspect.HostConfig.RestartPolicy.MaximumRetryCount,
		}
	}
	if created, err := time.Parse(time.RFC3339Nano, inspect.Created); err == nil {
		state.CreatedAt = created
	}
	if inspect.NetworkSettings != nil {
		state.IPAddress = firstNetworkIP(inspect.NetworkSettings.Networks)
		state.Network, state.Aliases = firstNetworkAttachment(inspect.NetworkSettings.Networks, inspect.ID, state.Name)
	}
	if inspect.State != nil {
		state.Running = inspect.State.Running
		state.ExitCode = inspect.State.ExitCode
		state.OOMKilled = inspect.State.OOMKilled
		if inspect.State.Health != nil {
			state.Health = runtime.HealthStatus(inspect.State.Health.Status)
		}
	}
	return state
}

func firstNetworkAttachment(networks map[string]*network.EndpointSettings, id string, containerName string) (string, []string) {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if endpoint := networks[name]; endpoint != nil {
			return name, customNetworkAliases(endpoint.Aliases, id, containerName)
		}
	}
	return "", nil
}

func customNetworkAliases(aliases []string, id string, containerName string) []string {
	shortID := id
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	custom := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if alias == id || alias == shortID || alias == containerName {
			continue
		}
		custom = append(custom, alias)
	}
	return custom
}

func sameAliasSet(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstNetworkIP(networks map[string]*network.EndpointSettings) string {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if endpoint := networks[name]; endpoint != nil && endpoint.IPAddress != "" {
			return endpoint.IPAddress
		}
	}
	return ""
}

func mapEvent(event events.Message) (runtime.RuntimeEvent, bool) {
	if event.Type != events.ContainerEventType {
		return runtime.RuntimeEvent{}, false
	}

	mappedType, ok := mapEventType(event.Action)
	if !ok {
		return runtime.RuntimeEvent{}, false
	}
	labels := eventLabels(event.Actor.Attributes)
	exitCode, _ := strconv.Atoi(event.Actor.Attributes["exitCode"])
	return runtime.RuntimeEvent{
		Type:        mappedType,
		ContainerID: runtime.ContainerID(event.Actor.ID),
		Name:        event.Actor.Attributes["name"],
		Labels:      labels,
		ExitCode:    exitCode,
		OOMKilled:   mappedType == runtime.EventOOM,
	}, true
}

func mapEventType(action events.Action) (runtime.RuntimeEventType, bool) {
	switch action {
	case events.ActionDie:
		return runtime.EventDie, true
	case events.ActionStop:
		return runtime.EventStop, true
	case events.ActionOOM:
		return runtime.EventOOM, true
	case events.ActionStart:
		return runtime.EventStart, true
	case events.ActionDestroy, events.ActionRemove:
		return runtime.EventDestroy, true
	default:
		return "", false
	}
}

func eventLabels(attributes map[string]string) map[string]string {
	labels := map[string]string{}
	for key, value := range attributes {
		if key == "name" || key == "image" || key == "exitCode" {
			continue
		}
		labels[key] = value
	}
	if len(labels) == 0 {
		return nil
	}
	return labels
}

func mapFromConfig(config *container.Config) map[string]string {
	if config == nil {
		return nil
	}
	return copyStringMap(config.Labels)
}

func copyStringMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]string, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
