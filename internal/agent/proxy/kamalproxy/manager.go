package kamalproxy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/runtime"
)

const (
	// DefaultImage pins kamal-proxy v0.8.4 by digest so a moved tag cannot
	// change the proxy that fronts every service.
	DefaultImage         = "basecamp/kamal-proxy:v0.8.4@sha256:b46ad3ec565dea7f2a36ebc7837a6cbc0996fa1735db5429f9d2301d292c60fb"
	DefaultContainerName = "serve-proxy"
	DefaultDeployTimeout = 30 * time.Second
	DefaultDrainTimeout  = 30 * time.Second

	// restoreGrace bounds a legacy-route restore beyond its deploy timeout.
	restoreGrace = 10 * time.Second
)

type Options struct {
	Image         string
	ContainerName string
	Network       string
	DeployTimeout time.Duration
	DrainTimeout  time.Duration
}

// Manager implements proxy.Manager by driving a kamal-proxy container
// through docker exec. It boots the proxy container on first use (or adopts
// a running one). kamal-proxy routes a set of targets per service, so the
// manager tracks the full set per service role and deploys it atomically,
// skipping exec calls when the set is unchanged.
type Manager struct {
	runtime runtime.Runtime
	opts    Options

	mu sync.Mutex
	// routed maps kamal-proxy service name to its target set. A present
	// key with an empty routedService means the service is known-removed.
	routed  map[string]routedService
	proxyID runtime.ContainerID
}

type routedService struct {
	addresses  []string // sorted
	healthPath string
	route      proxy.RouteOptions
	// legacyName is the pre-destination route name ("service-role") this
	// route may need to take over from on upgraded hosts.
	legacyName string
}

func New(rt runtime.Runtime, opts Options) *Manager {
	if opts.Image == "" {
		opts.Image = DefaultImage
	}
	if opts.ContainerName == "" {
		opts.ContainerName = DefaultContainerName
	}
	if opts.DeployTimeout == 0 {
		opts.DeployTimeout = DefaultDeployTimeout
	}
	if opts.DrainTimeout == 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	return &Manager{runtime: rt, opts: opts, routed: map[string]routedService{}}
}

func (m *Manager) AddTarget(ctx context.Context, target proxy.Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	service := serviceKey(target.Service, target.Destination, target.Role)
	current, known := m.routed[service]
	next := routedService{
		addresses:  addAddress(current.addresses, address(target)),
		healthPath: firstNonEmpty(target.HealthPath, current.healthPath),
		route:      current.route,
		legacyName: legacyServiceKey(target.Service, target.Role),
	}
	return m.apply(ctx, service, current, next, !known)
}

func (m *Manager) RemoveTarget(ctx context.Context, target proxy.Target) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	service := serviceKey(target.Service, target.Destination, target.Role)
	current, known := m.routed[service]
	if !known {
		return nil
	}
	next := routedService{
		addresses:  removeAddress(current.addresses, address(target)),
		healthPath: current.healthPath,
		route:      current.route,
		legacyName: current.legacyName,
	}
	// Never let a single-target removal empty the route. Deleting it would
	// free its hostnames, and kamal-proxy then sends them to any host-less
	// catch-all route of another service. Keeping the dead target returns
	// 502 until healing recreates it (Docker DNS resolves the new address).
	// Explicit removal goes through SetTargets with an empty set.
	if len(next.addresses) == 0 {
		return nil
	}
	return m.apply(ctx, service, current, next, false)
}

func (m *Manager) SetTargets(ctx context.Context, service string, destination string, role string, targets []proxy.Target, opts proxy.RouteOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := serviceKey(service, destination, role)
	current, known := m.routed[key]
	opts.Hosts = append([]string(nil), opts.Hosts...)
	sort.Strings(opts.Hosts)
	next := routedService{route: opts, legacyName: legacyServiceKey(service, role)}
	for _, target := range targets {
		next.addresses = addAddress(next.addresses, address(target))
		next.healthPath = firstNonEmpty(target.HealthPath, next.healthPath)
	}
	return m.apply(ctx, key, current, next, !known)
}

// apply reconciles the routed set for one kamal-proxy service. force issues
// the proxy call even when the set looks unchanged (used when the manager
// has no knowledge of the service yet, e.g. right after an agent restart).
// Callers must hold m.mu.
func (m *Manager) apply(ctx context.Context, service string, current routedService, next routedService, force bool) error {
	previousProxyID := m.proxyID
	proxyID, err := m.ensureProxy(ctx)
	if err != nil {
		return err
	}
	if !force && sameRoutedService(current, next) && previousProxyID != "" && previousProxyID == proxyID {
		return nil
	}

	if len(next.addresses) == 0 {
		if err := m.ensureRouteAbsent(ctx, proxyID, service); err != nil {
			return err
		}
		m.routed[service] = routedService{healthPath: next.healthPath}
		return nil
	}

	cmd := []string{"kamal-proxy", "deploy", service}
	for _, addr := range next.addresses {
		cmd = append(cmd, "--target="+addr)
	}
	for _, host := range next.route.Hosts {
		cmd = append(cmd, "--host="+host)
	}
	if next.healthPath != "" {
		cmd = append(cmd, "--health-check-path="+next.healthPath)
	}
	if next.route.TLS {
		cmd = append(cmd, "--tls")
	}
	deployTimeout := m.opts.DeployTimeout
	if next.route.DeployTimeout > 0 {
		deployTimeout = next.route.DeployTimeout
	}
	drainTimeout := m.opts.DrainTimeout
	if next.route.DrainTimeout > 0 {
		drainTimeout = next.route.DrainTimeout
	}
	cmd = append(cmd,
		"--deploy-timeout="+formatTimeout(deployTimeout),
		"--drain-timeout="+formatTimeout(drainTimeout),
	)
	migrated, err := m.deploy(ctx, proxyID, service, next.legacyName, cmd, deployTimeout)
	if err != nil {
		return err
	}
	if !migrated {
		// Routed under the legacy name; forget the route so the next apply
		// retries the migration.
		delete(m.routed, service)
		return nil
	}
	m.routed[service] = next
	return nil
}

// deploy runs a kamal-proxy deploy. Routes deployed by older agents were
// named "service-role" and may still hold this route's hostnames (or the
// host-less catch-all). kamal-proxy rejects the new name with a host
// conflict in that case, so the legacy route is removed and the deploy
// retried once. A conflict that remains is a genuine one and is reported.
//
// kamal-proxy reports a conflict only after the targets pass their health
// check, so if the retry fails the same command is replayed under the legacy
// name rather than leaving the hostnames unrouted. That restore ignores
// cancellation of ctx. It reports migrated=false when the route was left
// under the legacy name.
func (m *Manager) deploy(ctx context.Context, proxyID runtime.ContainerID, service string, legacyName string, cmd []string, deployTimeout time.Duration) (bool, error) {
	_, err := m.runtime.ExecContainer(ctx, proxyID, cmd)
	if err == nil {
		return true, nil
	}
	if legacyName == "" || !isHostConflict(err) {
		return false, fmt.Errorf("kamal-proxy deploy %s: %w", service, err)
	}
	removed, removeErr := m.removeRoute(ctx, proxyID, legacyName)
	if removeErr != nil {
		return false, errors.Join(fmt.Errorf("kamal-proxy deploy %s: %w", service, err), removeErr)
	}
	if _, err := m.runtime.ExecContainer(ctx, proxyID, cmd); err != nil {
		replaceErr := fmt.Errorf("kamal-proxy deploy %s: %w", service, err)
		if !removed {
			return false, replaceErr
		}
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deployTimeout+restoreGrace)
		defer cancel()
		restore := append([]string{cmd[0], cmd[1], legacyName}, cmd[3:]...)
		if _, restoreErr := m.runtime.ExecContainer(restoreCtx, proxyID, restore); restoreErr != nil {
			return false, errors.Join(replaceErr, fmt.Errorf("restore legacy route %s: %w", legacyName, restoreErr))
		}
		return false, nil
	}
	return true, nil
}

func isHostConflict(err error) bool {
	var exit *runtime.ExecExitError
	return errors.As(err, &exit) && strings.Contains(exit.Output, "host settings conflict with another service")
}

// ensureRouteAbsent consults the proxy itself, not the agent's volatile cache.
// The proxy may have restored routes from its persistent volume. Only the pinned
// CLI's specific not-found process exit is idempotent success; Docker failures
// and other proxy errors must remain failures (and must not be cached).
func (m *Manager) ensureRouteAbsent(ctx context.Context, id runtime.ContainerID, service string) error {
	_, err := m.removeRoute(ctx, id, service)
	return err
}

// removeRoute reports whether a route was actually removed.
func (m *Manager) removeRoute(ctx context.Context, id runtime.ContainerID, service string) (bool, error) {
	_, err := m.runtime.ExecContainer(ctx, id, []string{"kamal-proxy", "remove", service})
	if err == nil {
		return true, nil
	}
	var exit *runtime.ExecExitError
	if errors.As(err, &exit) && exit.Code == 1 && strings.TrimSpace(exit.Output) == "Error: service not found" {
		return false, nil
	}
	return false, fmt.Errorf("kamal-proxy remove %s: %w", service, err)
}

func sameRoutedService(a routedService, b routedService) bool {
	if !sameAddresses(a.addresses, b.addresses) || a.healthPath != b.healthPath {
		return false
	}
	if a.route.TLS != b.route.TLS || a.route.DeployTimeout != b.route.DeployTimeout || a.route.DrainTimeout != b.route.DrainTimeout {
		return false
	}
	return sameAddresses(a.route.Hosts, b.route.Hosts)
}

// ensureProxy adopts a running proxy container or boots a new one.
// Callers must hold m.mu.
func (m *Manager) ensureProxy(ctx context.Context) (runtime.ContainerID, error) {
	if m.proxyID != "" {
		state, err := m.runtime.InspectContainer(ctx, m.proxyID)
		if err == nil && state.Running {
			return m.proxyID, nil
		}
		m.proxyID = ""
	}

	states, err := m.runtime.ListContainers(ctx, runtime.ContainerFilters{Labels: map[string]string{
		"serve.managed":        "true",
		"serve.container_type": "proxy",
	}})
	if err != nil {
		return "", err
	}
	for _, state := range states {
		if state.Name != m.opts.ContainerName {
			continue
		}
		if state.Running {
			m.proxyID = state.ID
			return state.ID, nil
		}
		if err := m.runtime.RemoveContainer(ctx, state.ID); err != nil {
			return "", err
		}
	}

	// A pull failure is tolerated when the image is already present: the
	// registry being down must not keep the proxy from restarting.
	pullErr := m.runtime.PullImage(ctx, m.opts.Image)
	id, err := m.runtime.CreateContainer(ctx, runtime.ContainerSpec{
		Name:  m.opts.ContainerName,
		Image: m.opts.Image,
		Labels: map[string]string{
			"serve.managed":        "true",
			"serve.container_type": "proxy",
		},
		Ports: []runtime.Port{
			{Name: "http", ContainerPort: 80, HostPort: 80, HostIP: "0.0.0.0"},
			{Name: "https", ContainerPort: 443, HostPort: 443, HostIP: "0.0.0.0"},
		},
		Network: m.opts.Network,
		Restart: runtime.RestartPolicy{Policy: "unless-stopped"},
		Volumes: []string{"serve-proxy-data:/home/kamal-proxy/.config/kamal-proxy"},
	})
	if err != nil {
		if pullErr != nil {
			return "", errors.Join(fmt.Errorf("pull proxy image %s: %w", m.opts.Image, pullErr), fmt.Errorf("create proxy container: %w", err))
		}
		return "", fmt.Errorf("create proxy container: %w", err)
	}
	if err := m.runtime.StartContainer(ctx, id); err != nil {
		return "", fmt.Errorf("start proxy container: %w", err)
	}
	m.proxyID = id
	return id, nil
}

// serviceKey names the kamal-proxy route for one role of one service
// destination. Serve identifiers cannot contain "/", so the name is
// unambiguous; kamal-proxy accepts it as a service name.
func serviceKey(service string, destination string, role string) string {
	return service + "/" + destination + "/" + role
}

func legacyServiceKey(service string, role string) string {
	return service + "-" + role
}

func address(target proxy.Target) string {
	return fmt.Sprintf("%s:%d", target.ContainerName, target.Port)
}

func addAddress(addresses []string, addr string) []string {
	for _, existing := range addresses {
		if existing == addr {
			return append([]string(nil), addresses...)
		}
	}
	next := append(append([]string(nil), addresses...), addr)
	sort.Strings(next)
	return next
}

func removeAddress(addresses []string, addr string) []string {
	var next []string
	for _, existing := range addresses {
		if existing != addr {
			next = append(next, existing)
		}
	}
	return next
}

func sameAddresses(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// formatTimeout renders whole seconds for kamal-proxy, rounding up so a
// sub-second setting never becomes 0s.
func formatTimeout(d time.Duration) string {
	return fmt.Sprintf("%ds", int64(math.Ceil(d.Seconds())))
}
