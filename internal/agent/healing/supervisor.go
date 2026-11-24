package healing

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/uptimenine/serve/internal/agent/proxy"
	"github.com/uptimenine/serve/internal/agent/reconciler"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
)

// Defaults applied when a restart policy leaves the field unset, so a
// crash-looping workload cannot drive an unbounded pull/decrypt/recreate
// loop.
const (
	DefaultInitialBackoff = time.Second
	DefaultMaxBackoff     = time.Minute
	// DefaultRestartWindow is the quiet period after which the attempt
	// counter (and therefore the backoff) resets when no window is set.
	DefaultRestartWindow = 10 * time.Minute
)

type Supervisor struct {
	runtime      runtime.Runtime
	reconciler   *reconciler.Reconciler
	proxyManager proxy.Manager
	eventSink    EventSink
	sleeper      Sleeper
	now          func() time.Time

	// mu guards the restart bookkeeping only. The daemon heals services on
	// separate goroutines, so holding it across a heal would serialize them.
	mu          sync.Mutex
	attempts    map[string]int
	lastFailure map[string]time.Time
}

type Option func(*Supervisor)

type EventSink interface {
	Emit(ctx context.Context, event LifecycleEvent) error
}

type Sleeper interface {
	Sleep(ctx context.Context, duration time.Duration) error
}

type LifecycleEvent struct {
	Name        string
	Service     string
	Destination string
	Role        string
	Version     string
	Container   string
	ExitCode    int
	OOMKilled   bool
	Attempt     int
	Actor       string
}

type Result struct {
	Ignored         bool
	Restarted       bool
	MarkedUnhealthy bool
}

func NewSupervisor(rt runtime.Runtime, options ...Option) *Supervisor {
	s := &Supervisor{
		runtime:     rt,
		reconciler:  reconciler.New(rt),
		eventSink:   noopSink{},
		sleeper:     realSleeper{},
		now:         time.Now,
		attempts:    map[string]int{},
		lastFailure: map[string]time.Time{},
	}
	for _, option := range options {
		option(s)
	}
	return s
}

// WithReconciler replaces the supervisor's default reconciler, e.g. with one
// that can re-materialize secrets when recreating containers.
func WithReconciler(rec *reconciler.Reconciler) Option {
	return func(s *Supervisor) {
		if rec != nil {
			s.reconciler = rec
		}
	}
}

func WithProxyManager(manager proxy.Manager) Option {
	return func(s *Supervisor) {
		s.proxyManager = manager
	}
}

func WithEventSink(sink EventSink) Option {
	return func(s *Supervisor) {
		if sink != nil {
			s.eventSink = sink
		}
	}
}

func WithSleeper(sleeper Sleeper) Option {
	return func(s *Supervisor) {
		if sleeper != nil {
			s.sleeper = sleeper
		}
	}
}

func WithClock(now func() time.Time) Option {
	return func(s *Supervisor) {
		if now != nil {
			s.now = now
		}
	}
}

func (s *Supervisor) HandleRuntimeEvent(ctx context.Context, event runtime.RuntimeEvent, desired planner.DesiredState) (Result, error) {
	if !healableEvent(event.Type) {
		return Result{Ignored: true}, nil
	}

	container, ok := desiredContainer(event, desired)
	if !ok {
		return Result{Ignored: true}, nil
	}
	if !shouldRestart(container.Restart, event) {
		return Result{Ignored: true}, nil
	}

	window, err := windowDuration(container.Restart)
	if err != nil {
		return Result{}, err
	}
	attempt := s.recordFailure(container.Name, window)
	oomKilled := event.OOMKilled || event.Type == runtime.EventOOM

	if err := s.emit(ctx, "container_exited", desired, container, event, attempt, oomKilled); err != nil {
		return Result{}, err
	}

	if container.Restart.MaxAttempts > 0 && attempt > container.Restart.MaxAttempts {
		if err := s.emit(ctx, "restart_loop_detected", desired, container, event, attempt, oomKilled); err != nil {
			return Result{}, err
		}
		return Result{MarkedUnhealthy: true}, nil
	}

	if err := s.removeProxyTarget(ctx, desired.Service, desired.Destination, container); err != nil {
		return Result{}, err
	}
	if err := s.sleepBackoff(ctx, container.Restart, attempt); err != nil {
		return Result{}, err
	}
	if _, err := s.reconciler.Reconcile(ctx, desired); err != nil {
		return Result{}, err
	}
	if err := s.emit(ctx, "container_restarted", desired, container, event, attempt, oomKilled); err != nil {
		return Result{}, err
	}

	return Result{Restarted: true}, nil
}

// recordFailure counts a failure and returns its attempt number. A failure
// past the restart window is a fresh incident, not part of a restart loop,
// so the counter resets.
func (s *Supervisor) recordFailure(name string, window time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if last, ok := s.lastFailure[name]; ok && window > 0 && now.Sub(last) > window {
		s.attempts[name] = 0
	}
	s.lastFailure[name] = now
	s.attempts[name]++
	return s.attempts[name]
}

func healableEvent(eventType runtime.RuntimeEventType) bool {
	switch eventType {
	case runtime.EventDie, runtime.EventStop, runtime.EventOOM:
		return true
	default:
		return false
	}
}

func desiredContainer(event runtime.RuntimeEvent, desired planner.DesiredState) (planner.Container, bool) {
	for _, container := range desired.Containers {
		if event.Name != "" && container.Name == event.Name {
			return container, true
		}
		if event.Labels["serve.role"] == container.Role && event.Labels["serve.replica"] == fmt.Sprint(container.Replica) && event.Labels["serve.version"] == desired.Version {
			return container, true
		}
	}
	return planner.Container{}, false
}

func shouldRestart(restart planner.Restart, event runtime.RuntimeEvent) bool {
	if restart.Controller == "docker" {
		return false
	}
	switch restart.Policy {
	case "", "always", "unless-stopped":
		return true
	case "on-failure":
		return event.ExitCode != 0 || event.Type == runtime.EventOOM || event.OOMKilled
	case "no":
		return false
	default:
		return false
	}
}

func (s *Supervisor) removeProxyTarget(ctx context.Context, service string, destination string, container planner.Container) error {
	if !container.Proxy || s.proxyManager == nil {
		return nil
	}
	return s.proxyManager.RemoveTarget(ctx, proxy.Target{
		Service:       service,
		Destination:   destination,
		Role:          container.Role,
		ContainerName: container.Name,
		Port:          targetPort(container),
		HealthPath:    targetHealthPath(container),
	})
}

func (s *Supervisor) sleepBackoff(ctx context.Context, restart planner.Restart, attempt int) error {
	backoff, err := backoffDuration(restart, attempt)
	if err != nil {
		return err
	}
	if backoff == 0 {
		return nil
	}
	return s.sleeper.Sleep(ctx, backoff)
}

func windowDuration(restart planner.Restart) (time.Duration, error) {
	return durationOrDefault("restart window", restart.Window, DefaultRestartWindow)
}

// backoffDuration doubles from the initial backoff and is always capped, so
// it can neither be skipped by omission nor overflow on long restart loops.
// Without a configured initial_backoff the first restart is immediate (a
// one-off crash heals fast) and repeats back off from DefaultInitialBackoff.
// An explicit initial_backoff of 0 still disables the delay.
func backoffDuration(restart planner.Restart, attempt int) (time.Duration, error) {
	if restart.InitialBackoff == "" {
		if attempt <= 1 {
			return 0, nil
		}
		attempt--
	}
	initial, err := durationOrDefault("initial backoff", restart.InitialBackoff, DefaultInitialBackoff)
	if err != nil {
		return 0, err
	}
	maxBackoff, err := durationOrDefault("max backoff", restart.MaxBackoff, DefaultMaxBackoff)
	if err != nil {
		return 0, err
	}
	if initial <= 0 {
		return 0, nil
	}
	backoff := initial
	for i := 1; i < attempt && backoff < maxBackoff; i++ {
		backoff *= 2
	}
	return min(backoff, maxBackoff), nil
}

func durationOrDefault(name string, value string, fallback time.Duration) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, value, err)
	}
	return parsed, nil
}

func (s *Supervisor) emit(ctx context.Context, name string, desired planner.DesiredState, container planner.Container, event runtime.RuntimeEvent, attempt int, oomKilled bool) error {
	return s.eventSink.Emit(ctx, LifecycleEvent{
		Name:        name,
		Service:     desired.Service,
		Destination: desired.Destination,
		Role:        container.Role,
		Version:     desired.Version,
		Container:   container.Name,
		ExitCode:    event.ExitCode,
		OOMKilled:   oomKilled,
		Attempt:     attempt,
		Actor:       "serve-agent",
	})
}

func targetPort(container planner.Container) int {
	if container.Healthcheck != nil && container.Healthcheck.Port > 0 {
		return container.Healthcheck.Port
	}
	if len(container.Ports) > 0 {
		return container.Ports[0].ContainerPort
	}
	return 0
}

func targetHealthPath(container planner.Container) string {
	if container.Healthcheck != nil {
		return container.Healthcheck.Path
	}
	return ""
}

type noopSink struct{}

func (noopSink) Emit(ctx context.Context, event LifecycleEvent) error {
	_ = ctx
	_ = event
	return nil
}

type realSleeper struct{}

func (realSleeper) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
