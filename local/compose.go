// +build local

/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/compose-spec/compose-go/types"
	"github.com/docker/buildx/build"
	"github.com/docker/buildx/driver"
	px "github.com/docker/buildx/util/progress"
	moby "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/sanathkr/go-yaml"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose-cli/api/compose"
	"github.com/docker/compose-cli/api/containers"
	"github.com/docker/compose-cli/formatter"
	"github.com/docker/compose-cli/progress"
)

func (s *local) Up(ctx context.Context, project *types.Project, detach bool) error {
	for k, network := range project.Networks {
		if !network.External.External && network.Name != "" {
			network.Name = fmt.Sprintf("%s_%s", project.Name, k)
			project.Networks[k] = network
		}
		err := s.ensureNetwork(ctx, network)
		if err != nil {
			return err
		}
	}

	for k, volume := range project.Volumes {
		if !volume.External.External && volume.Name != "" {
			volume.Name = fmt.Sprintf("%s_%s", project.Name, k)
			project.Volumes[k] = volume
		}
		err := s.ensureVolume(ctx, volume)
		if err != nil {
			return err
		}
	}

	for _, service := range project.Services {
		err := s.applyPullPolicy(ctx, project, service)
		if err != nil {
			return err
		}
	}

	err := inDependencyOrder(ctx, project, func(c context.Context, service types.ServiceConfig) error {
		return s.ensureService(c, project, service)
	})
	return err
}

func getContainerName(c moby.Container) string {
	// Names return container canonical name /foo  + link aliases /linked_by/foo
	for _, name := range c.Names {
		if strings.LastIndex(name, "/") == 0 {
			return name[1:]
		}
	}
	return c.Names[0][1:]
}

func (s *local) applyPullPolicy(ctx context.Context, project *types.Project, service types.ServiceConfig) error {
	w := progress.ContextWriter(ctx)
	// TODO build vs pull should be controlled by pull policy
	opts := map[string]build.Options{}
	if service.Build != nil {
		opts[service.Name] = s.buildImage(ctx, service, project.WorkingDir)
	}

	if len(opts) > 0 {
		w := px.NewPrinter(ctx, os.Stdout, "auto")
		const drivername = "buildx_buildkit_default"
		d, err := driver.GetDriver(ctx, drivername, nil, s.containerService.apiClient, nil, nil, "", nil, project.WorkingDir)
		if err != nil {
			return err
		}
		driverInfo := []build.DriverInfo{
			{
				Name:   "default",
				Driver: d,
			},
		}
		// We rely on buildx "docker" builder integrated in docker engine, so don't need a DockerAPI here
		// FIXME pass auth file from
		_, err = build.Build(ctx, driverInfo, opts, nil, nil, w)
		return err
	}

	if service.Image != "" {
		_, _, err := s.containerService.apiClient.ImageInspectWithRaw(ctx, service.Image)
		if err != nil {
			if errdefs.IsNotFound(err) {
				stream, err := s.containerService.apiClient.ImagePull(ctx, service.Image, moby.ImagePullOptions{})
				if err != nil {
					return err
				}
				dec := json.NewDecoder(stream)
				for {
					var jm jsonmessage.JSONMessage
					if err := dec.Decode(&jm); err != nil {
						if err == io.EOF {
							break
						}
						return err
					}
					toProgressEvent(jm, w)
				}
			}
		}
	}
	return nil
}

func toProgressEvent(jm jsonmessage.JSONMessage, w progress.Writer) {
	if jm.Progress != nil {
		if jm.Progress.Total != 0 {
			status := progress.Working
			if jm.Status == "Pull complete" {
				status = progress.Done
			}
			w.Event(progress.Event{
				ID:         jm.ID,
				Text:       jm.Status,
				Status:     status,
				StatusText: jm.Progress.String(),
			})
		} else {
			if jm.Error != nil {
				w.Event(progress.Event{
					ID:         jm.ID,
					Text:       jm.Status,
					Status:     progress.Error,
					StatusText: jm.Error.Message,
				})
			} else if jm.Status == "Pull complete" || jm.Status == "Already exists" {
				w.Event(progress.Event{
					ID:     jm.ID,
					Text:   jm.Status,
					Status: progress.Done,
				})
			} else {
				w.Event(progress.Event{
					ID:     jm.ID,
					Text:   jm.Status,
					Status: progress.Working,
				})
			}
		}
	}
}

func (s *local) Down(ctx context.Context, projectName string) error {
	list, err := s.containerService.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(projectName),
		),
	})
	if err != nil {
		return err
	}

	eg, _ := errgroup.WithContext(ctx)
	w := progress.ContextWriter(ctx)
	for _, c := range list {
		container := c
		eg.Go(func() error {
			w.Event(progress.Event{
				ID:     getContainerName(container),
				Text:   "Stopping",
				Status: progress.Working,
			})
			err := s.containerService.Stop(ctx, container.ID, nil)
			if err != nil {
				return err
			}
			w.Event(progress.Event{
				ID:     getContainerName(container),
				Text:   "Removing",
				Status: progress.Working,
			})
			err = s.containerService.Delete(ctx, container.ID, containers.DeleteRequest{})
			if err != nil {
				return err
			}
			w.Event(progress.Event{
				ID:     getContainerName(container),
				Text:   "Removed",
				Status: progress.Done,
			})
			return nil
		})
	}
	return eg.Wait()
}

func (s *local) Logs(ctx context.Context, projectName string, w io.Writer) error {
	list, err := s.containerService.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(projectName),
		),
	})
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	consumer := formatter.NewLogConsumer(w)
	for _, c := range list {
		service := c.Labels[serviceLabel]
		containerID := c.ID
		go func() {
			_ = s.containerService.Logs(ctx, containerID, containers.LogsRequest{
				Follow: true,
				Writer: consumer.GetWriter(service, containerID),
			})
			wg.Done()
		}()
		wg.Add(1)
	}
	wg.Wait()
	return nil
}

func (s *local) Ps(ctx context.Context, projectName string) ([]compose.ServiceStatus, error) {
	list, err := s.containerService.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(projectName),
		),
	})
	if err != nil {
		return nil, err
	}
	return containersToServiceStatus(list)
}

func containersToServiceStatus(containers []moby.Container) ([]compose.ServiceStatus, error) {
	containersByLabel, keys, err := groupContainerByLabel(containers, serviceLabel)
	if err != nil {
		return nil, err
	}
	var services []compose.ServiceStatus
	for _, service := range keys {
		containers := containersByLabel[service]
		runnningContainers := []moby.Container{}
		for _, container := range containers {
			if container.State == "running" {
				runnningContainers = append(runnningContainers, container)
			}
		}
		services = append(services, compose.ServiceStatus{
			ID:       service,
			Name:     service,
			Desired:  len(containers),
			Replicas: len(runnningContainers),
		})
	}
	return services, nil
}

func groupContainerByLabel(containers []moby.Container, labelName string) (map[string][]moby.Container, []string, error) {
	containersByLabel := map[string][]moby.Container{}
	keys := []string{}
	for _, c := range containers {
		label, ok := c.Labels[labelName]
		if !ok {
			return nil, nil, fmt.Errorf("No label %q set on container %q of compose project", labelName, c.ID)
		}
		labelContainers, ok := containersByLabel[label]
		if !ok {
			labelContainers = []moby.Container{}
			keys = append(keys, label)
		}
		labelContainers = append(labelContainers, c)
		containersByLabel[label] = labelContainers
	}
	sort.Strings(keys)
	return containersByLabel, keys, nil
}

func (s *local) List(ctx context.Context, projectName string) ([]compose.Stack, error) {
	list, err := s.containerService.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(hasProjectLabelFilter()),
	})
	if err != nil {
		return nil, err
	}

	return containersToStacks(list)
}

func containersToStacks(containers []moby.Container) ([]compose.Stack, error) {
	containersByLabel, keys, err := groupContainerByLabel(containers, projectLabel)
	if err != nil {
		return nil, err
	}
	var projects []compose.Stack
	for _, project := range keys {
		projects = append(projects, compose.Stack{
			ID:     project,
			Name:   project,
			Status: combinedStatus(containerToState(containersByLabel[project])),
		})
	}
	return projects, nil
}

func containerToState(containers []moby.Container) []string {
	statuses := []string{}
	for _, c := range containers {
		statuses = append(statuses, c.State)
	}
	return statuses
}

func combinedStatus(statuses []string) string {
	nbByStatus := map[string]int{}
	keys := []string{}
	for _, status := range statuses {
		nb, ok := nbByStatus[status]
		if !ok {
			nb = 0
			keys = append(keys, status)
		}
		nbByStatus[status] = nb + 1
	}
	sort.Strings(keys)
	result := ""
	for _, status := range keys {
		nb := nbByStatus[status]
		if result != "" {
			result = result + ", "
		}
		result = result + fmt.Sprintf("%s(%d)", status, nb)
	}
	return result
}

func (s *local) Convert(ctx context.Context, project *types.Project, format string) ([]byte, error) {
	switch format {
	case "json":
		return json.MarshalIndent(project, "", "  ")
	case "yaml":
		return yaml.Marshal(project)
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func getContainerCreateOptions(p *types.Project, s types.ServiceConfig, number int, inherit *moby.Container) (*container.Config, *container.HostConfig, *network.NetworkingConfig, error) {
	hash, err := jsonHash(s)
	if err != nil {
		return nil, nil, nil, err
	}
	labels := map[string]string{
		projectLabel:         p.Name,
		serviceLabel:         s.Name,
		configHashLabel:      hash,
		containerNumberLabel: strconv.Itoa(number),
	}

	var (
		runCmd     strslice.StrSlice
		entrypoint strslice.StrSlice
	)
	if len(s.Command) > 0 {
		runCmd = strslice.StrSlice(s.Command)
	}
	if len(s.Entrypoint) > 0 {
		entrypoint = strslice.StrSlice(s.Entrypoint)
	}
	image := s.Image
	if s.Image == "" {
		image = fmt.Sprintf("%s_%s", p.Name, s.Name)
	}

	var (
		tty         = s.Tty
		stdinOpen   = s.StdinOpen
		attachStdin = false
	)

	containerConfig := container.Config{
		Hostname:        s.Hostname,
		Domainname:      s.DomainName,
		User:            s.User,
		ExposedPorts:    buildContainerPorts(s),
		Tty:             tty,
		OpenStdin:       stdinOpen,
		StdinOnce:       true,
		AttachStdin:     attachStdin,
		AttachStderr:    true,
		AttachStdout:    true,
		Cmd:             runCmd,
		Image:           image,
		WorkingDir:      s.WorkingDir,
		Entrypoint:      entrypoint,
		NetworkDisabled: s.NetworkMode == "disabled",
		MacAddress:      s.MacAddress,
		Labels:          labels,
		StopSignal:      s.StopSignal,
		Env:             toMobyEnv(s.Environment),
		Healthcheck:     toMobyHealthCheck(s.HealthCheck),
		// Volumes:         // FIXME unclear to me the overlap with HostConfig.Mounts
		StopTimeout: toSeconds(s.StopGracePeriod),
	}

	mountOptions := buildContainerMountOptions(p, s, inherit)
	bindings := buildContainerBindingOptions(s)

	networkMode := getNetworkMode(p, s)
	hostConfig := container.HostConfig{
		Mounts:         mountOptions,
		CapAdd:         strslice.StrSlice(s.CapAdd),
		CapDrop:        strslice.StrSlice(s.CapDrop),
		NetworkMode:    networkMode,
		Init:           s.Init,
		ReadonlyRootfs: s.ReadOnly,
		// ShmSize: , TODO
		Sysctls:      s.Sysctls,
		PortBindings: bindings,
	}

	networkConfig := buildDefaultNetworkConfig(s, networkMode)
	return &containerConfig, &hostConfig, networkConfig, nil
}

func buildContainerPorts(s types.ServiceConfig) nat.PortSet {
	ports := nat.PortSet{}
	for _, p := range s.Ports {
		p := nat.Port(fmt.Sprintf("%d/%s", p.Target, p.Protocol))
		ports[p] = struct{}{}
	}
	return ports
}

func buildContainerBindingOptions(s types.ServiceConfig) nat.PortMap {
	bindings := nat.PortMap{}
	for _, port := range s.Ports {
		p := nat.Port(fmt.Sprintf("%d/%s", port.Target, port.Protocol))
		bind := []nat.PortBinding{}
		binding := nat.PortBinding{}
		if port.Published > 0 {
			binding.HostPort = fmt.Sprint(port.Published)
		}
		bind = append(bind, binding)
		bindings[p] = bind
	}
	return bindings
}

func buildContainerMountOptions(p *types.Project, s types.ServiceConfig, inherit *moby.Container) []mount.Mount {
	mounts := []mount.Mount{}
	var inherited []string
	if inherit != nil {
		for _, m := range inherit.Mounts {
			if m.Type == "tmpfs" {
				continue
			}
			src := m.Source
			if m.Type == "volume" {
				src = m.Name
			}
			mounts = append(mounts, mount.Mount{
				Type:     m.Type,
				Source:   src,
				Target:   m.Destination,
				ReadOnly: !m.RW,
			})
			inherited = append(inherited, m.Destination)
		}
	}

	for _, v := range s.Volumes {
		if contains(inherited, v.Target) {
			continue
		}
		source := v.Source
		if v.Type == "bind" && !filepath.IsAbs(source) {
			// FIXME handle ~/
			source = filepath.Join(p.WorkingDir, source)
		}

		mounts = append(mounts, mount.Mount{
			Type:          mount.Type(v.Type),
			Source:        source,
			Target:        v.Target,
			ReadOnly:      v.ReadOnly,
			Consistency:   mount.Consistency(v.Consistency),
			BindOptions:   buildBindOption(v.Bind),
			VolumeOptions: buildVolumeOptions(v.Volume),
			TmpfsOptions:  buildTmpfsOptions(v.Tmpfs),
		})
	}
	return mounts
}

func buildBindOption(bind *types.ServiceVolumeBind) *mount.BindOptions {
	if bind == nil {
		return nil
	}
	return &mount.BindOptions{
		Propagation: mount.Propagation(bind.Propagation),
		// NonRecursive: false, FIXME missing from model ?
	}
}

func buildVolumeOptions(vol *types.ServiceVolumeVolume) *mount.VolumeOptions {
	if vol == nil {
		return nil
	}
	return &mount.VolumeOptions{
		NoCopy: vol.NoCopy,
		// Labels:       , // FIXME missing from model ?
		// DriverConfig: , // FIXME missing from model ?
	}
}

func buildTmpfsOptions(tmpfs *types.ServiceVolumeTmpfs) *mount.TmpfsOptions {
	if tmpfs == nil {
		return nil
	}
	return &mount.TmpfsOptions{
		SizeBytes: tmpfs.Size,
		// Mode:      , // FIXME missing from model ?
	}
}

func buildDefaultNetworkConfig(s types.ServiceConfig, networkMode container.NetworkMode) *network.NetworkingConfig {
	config := map[string]*network.EndpointSettings{}
	net := string(networkMode)
	config[net] = &network.EndpointSettings{
		Aliases: getAliases(s, s.Networks[net]),
	}

	return &network.NetworkingConfig{
		EndpointsConfig: config,
	}
}

func getAliases(s types.ServiceConfig, c *types.ServiceNetworkConfig) []string {
	aliases := []string{s.Name}
	if c != nil {
		aliases = append(aliases, c.Aliases...)
	}
	return aliases
}

func getNetworkMode(p *types.Project, service types.ServiceConfig) container.NetworkMode {
	mode := service.NetworkMode
	if mode == "" {
		if len(p.Networks) > 0 {
			for name := range getNetworksForService(service) {
				return container.NetworkMode(p.Networks[name].Name)
			}
		}
		return container.NetworkMode("none")
	}

	/// FIXME incomplete implementation
	if strings.HasPrefix(mode, "service:") {
		panic("Not yet implemented")
	}
	if strings.HasPrefix(mode, "container:") {
		panic("Not yet implemented")
	}

	return container.NetworkMode(mode)
}

func getNetworksForService(s types.ServiceConfig) map[string]*types.ServiceNetworkConfig {
	if len(s.Networks) > 0 {
		return s.Networks
	}
	return map[string]*types.ServiceNetworkConfig{"default": nil}
}

func (s *local) ensureNetwork(ctx context.Context, n types.NetworkConfig) error {
	_, err := s.containerService.apiClient.NetworkInspect(ctx, n.Name, moby.NetworkInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			createOpts := moby.NetworkCreate{
				// TODO NameSpace Labels
				Labels:     n.Labels,
				Driver:     n.Driver,
				Options:    n.DriverOpts,
				Internal:   n.Internal,
				Attachable: n.Attachable,
			}

			if n.Ipam.Driver != "" || len(n.Ipam.Config) > 0 {
				createOpts.IPAM = &network.IPAM{}
			}

			if n.Ipam.Driver != "" {
				createOpts.IPAM.Driver = n.Ipam.Driver
			}

			for _, ipamConfig := range n.Ipam.Config {
				config := network.IPAMConfig{
					Subnet: ipamConfig.Subnet,
				}
				createOpts.IPAM.Config = append(createOpts.IPAM.Config, config)
			}
			w := progress.ContextWriter(ctx)
			w.Event(progress.Event{
				ID:         fmt.Sprintf("Network %q", n.Name),
				Status:     progress.Working,
				StatusText: "Create",
			})
			if _, err := s.containerService.apiClient.NetworkCreate(ctx, n.Name, createOpts); err != nil {
				return errors.Wrapf(err, "failed to create network %s", n.Name)
			}
			w.Event(progress.Event{
				ID:         fmt.Sprintf("Network %q", n.Name),
				Status:     progress.Done,
				StatusText: "Created",
			})
			return nil
		}
		return err
	}
	return nil
}

func (s *local) ensureVolume(ctx context.Context, volume types.VolumeConfig) error {
	// TODO could identify volume by label vs name
	_, err := s.volumeService.Inspect(ctx, volume.Name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			w := progress.ContextWriter(ctx)
			w.Event(progress.Event{
				ID:         fmt.Sprintf("Volume %q", volume.Name),
				Status:     progress.Working,
				StatusText: "Create",
			})
			// TODO we miss support for driver_opts and labels
			_, err := s.volumeService.Create(ctx, volume.Name, nil)
			w.Event(progress.Event{
				ID:         fmt.Sprintf("Volume %q", volume.Name),
				Status:     progress.Done,
				StatusText: "Created",
			})
			if err != nil {
				return err
			}
		}
		return err
	}
	return nil
}
