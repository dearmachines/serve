package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/uptimenine/serve/internal/agent/events"
	"github.com/uptimenine/serve/internal/agent/healing"
	"github.com/uptimenine/serve/internal/agent/proxy"
	agentstate "github.com/uptimenine/serve/internal/agent/state"
	"github.com/uptimenine/serve/internal/planner"
	"github.com/uptimenine/serve/internal/runtime"
)

type ExecRequest struct {
	Container string   `json:"container"`
	Command   []string `json:"command"`
}
type ExecResponse struct {
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}
type RemoveRequest struct {
	Force       bool   `json:"force"`
	Service     string `json:"service,omitempty"`
	Destination string `json:"destination,omitempty"`
	Role        string `json:"role,omitempty"`
}
type PruneRequest struct {
	Force bool `json:"force"`
}

type RollbackRequest struct {
	Service     string `json:"service"`
	Destination string `json:"destination"`
}

// RollbackResponse distinguishes a completed rollback from an accepted request.
// Lifecycle events are emitted immediately on the agent; Output preserves the
// CLI's event transcript even when the operation fails.
type RollbackResponse struct {
	Status string `json:"status"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

func decodeRequest(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "expected exactly one JSON request", http.StatusBadRequest)
		return false
	}
	return true
}

func (d *Daemon) managedContainers(ctx context.Context, labels map[string]string) ([]runtime.ContainerState, error) {
	filters := map[string]string{"serve.managed": "true"}
	for key, value := range labels {
		if value != "" {
			filters[key] = value
		}
	}
	containers, err := d.runtime.ListContainers(ctx, runtime.ContainerFilters{Labels: filters})
	sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
	return containers, err
}

var (
	errContainerNotFound  = errors.New("container not found")
	errAmbiguousSelection = errors.New("multiple matching containers found; pass --container")
)

func selectionErrorStatus(err error) int {
	if errors.Is(err, errContainerNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, errAmbiguousSelection) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (d *Daemon) selectContainer(ctx context.Context, name string, labels map[string]string) (runtime.ContainerState, error) {
	containers, err := d.managedContainers(ctx, labels)
	if err != nil {
		return runtime.ContainerState{}, err
	}
	if name != "" {
		for _, c := range containers {
			if c.Name == name || string(c.ID) == name {
				return c, nil
			}
		}
		return runtime.ContainerState{}, fmt.Errorf("%w: %s", errContainerNotFound, name)
	}
	if len(containers) == 0 {
		return runtime.ContainerState{}, fmt.Errorf("%w: no matching Serve-managed containers", errContainerNotFound)
	}
	if len(containers) > 1 {
		return runtime.ContainerState{}, errAmbiguousSelection
	}
	return containers[0], nil
}
func (d *Daemon) handleExec(w http.ResponseWriter, r *http.Request) {
	var request ExecRequest
	if !decodeRequest(w, r, &request) {
		return
	}
	if request.Container == "" || len(request.Command) == 0 {
		http.Error(w, "container and command are required", 400)
		return
	}
	d.maintenance.RLock()
	defer d.maintenance.RUnlock()
	c, err := d.selectContainer(r.Context(), request.Container, nil)
	if err != nil {
		http.Error(w, err.Error(), selectionErrorStatus(err))
		return
	}
	lock := d.operationLock(stateKey(c.Labels["serve.service"], c.Labels["serve.destination"]))
	lock.Lock()
	defer lock.Unlock()
	c, err = d.selectContainer(r.Context(), request.Container, nil)
	if err != nil {
		http.Error(w, err.Error(), selectionErrorStatus(err))
		return
	}
	output, err := d.runtime.ExecContainer(r.Context(), c.ID, request.Command)
	result := ExecResponse{Output: output}
	if err != nil {
		result.Error = err.Error()
	}
	writeJSON(w, result)
}
func (d *Daemon) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if _, err := d.runtime.ListContainers(r.Context(), runtime.ContainerFilters{}); err != nil {
		http.Error(w, "Docker reachable: failed: "+err.Error(), 500)
		return
	}
	if err := d.runtime.CreateNetwork(r.Context(), runtime.NetworkSpec{Name: "serve"}); err != nil {
		http.Error(w, "serve network: failed: "+err.Error(), 500)
		return
	}
	fmt.Fprintln(w, "Docker reachable: ok\nserve network: ok")
}
func (d *Daemon) handleRemove(w http.ResponseWriter, r *http.Request) {
	var request RemoveRequest
	if !decodeRequest(w, r, &request) {
		return
	}
	if !request.Force {
		http.Error(w, "force is required", 400)
		return
	}
	d.maintenance.Lock()
	defer d.maintenance.Unlock()
	if err := d.removeIntent(r.Context(), request); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	containers, err := d.managedContainers(r.Context(), map[string]string{"serve.service": request.Service, "serve.destination": request.Destination, "serve.role": request.Role})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	removed := 0
	for _, c := range containers {
		if c.Labels["serve.container_type"] == "proxy" {
			continue
		}
		if c.Running {
			if err := d.runtime.StopContainer(r.Context(), c.ID, time.Second); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		if err := d.runtime.RemoveContainer(r.Context(), c.ID); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		removed++
	}
	states, err := d.store.ListDesired()
	if err != nil {
		maintenanceRefreshError(w, "Removed", removed, err)
		return
	}
	for _, desired := range states {
		if request.Service != "" && request.Service != desired.Service {
			continue
		}
		if request.Destination != "" && request.Destination != desired.Destination {
			continue
		}
		actual, err := d.actualState(r.Context(), desired.Service, desired.Destination)
		if err == nil {
			err = d.store.SaveActual(actual)
		}
		if err != nil {
			maintenanceRefreshError(w, "Removed", removed, err)
			return
		}
	}
	fmt.Fprintf(w, "Removed %d container(s)\n", removed)
}

// removeIntent runs under the exclusive maintenance lock. Unroute affected
// roles and persist removal before deleting containers, so queued events and
// restarts cannot resurrect them.
func (d *Daemon) removeIntent(ctx context.Context, request RemoveRequest) error {
	states, err := d.store.ListDesired()
	if err != nil {
		return err
	}
	for _, desired := range states {
		if request.Service != "" && request.Service != desired.Service {
			continue
		}
		if request.Destination != "" && request.Destination != desired.Destination {
			continue
		}
		remaining := make([]planner.Container, 0, len(desired.Containers))
		routes := map[string]bool{}
		for _, container := range desired.Containers {
			if request.Role != "" && request.Role != container.Role {
				remaining = append(remaining, container)
				continue
			}
			if container.Proxy {
				routes[container.Role] = true
			}
		}
		if len(remaining) == len(desired.Containers) {
			// Older agents could publish reduced desired state before failing to
			// update last-good. Repair that pair on retry without resetting an
			// unrelated rollback baseline for a genuine no-op removal.
			baseline, err := d.store.LoadLastGood(desired.Service, desired.Destination)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("load removal rollback baseline: %w", err)
			}
			stale := false
			for _, container := range baseline.Containers {
				if request.Role == "" || container.Role == request.Role {
					stale = true
					if container.Proxy {
						routes[container.Role] = true
					}
				}
			}
			if !stale {
				continue
			}
		}
		for role := range routes {
			if err := d.proxy.SetTargets(ctx, desired.Service, role, nil, proxy.RouteOptions{}); err != nil {
				return err
			}
		}
		desired.Containers = remaining
		if len(remaining) == 0 {
			if err := d.store.Forget(desired.Service, desired.Destination); err != nil {
				return err
			}
			d.mu.Lock()
			delete(d.desired, stateKey(desired.Service, desired.Destination))
			d.mu.Unlock()
		} else {
			// Prepare the rollback baseline first. If either write fails, desired
			// still contains the role and a retry repeats the baseline update.
			if err := d.store.SaveLastGood(desired); err != nil {
				return fmt.Errorf("save removal rollback baseline: %w", err)
			}
			if err := d.store.SaveDesired(desired); err != nil {
				return fmt.Errorf("save removal desired state: %w", err)
			}
			d.setDesired(desired)
		}
	}
	return nil
}

func (d *Daemon) handlePrune(w http.ResponseWriter, r *http.Request) {
	var request PruneRequest
	if !decodeRequest(w, r, &request) {
		return
	}
	if !request.Force {
		http.Error(w, "force is required", 400)
		return
	}
	d.maintenance.Lock()
	defer d.maintenance.Unlock()
	containers, err := d.managedContainers(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	states, err := d.store.ListDesired()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	active := map[string]bool{}
	for _, desired := range states {
		for _, c := range desired.Containers {
			active[c.Name] = true
		}
	}
	count := 0
	for _, c := range containers {
		if c.Running || c.Labels["serve.container_type"] == "proxy" || active[c.Name] {
			continue
		}
		if err := d.runtime.RemoveContainer(r.Context(), c.ID); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		count++
	}
	for _, desired := range states {
		actual, err := d.actualState(r.Context(), desired.Service, desired.Destination)
		if err == nil {
			err = d.store.SaveActual(actual)
		}
		if err != nil {
			maintenanceRefreshError(w, "Pruned", count, err)
			return
		}
	}
	fmt.Fprintf(w, "Pruned %d container(s)\n", count)
}
func maintenanceRefreshError(w http.ResponseWriter, verb string, count int, err error) {
	http.Error(w, fmt.Sprintf("%s %d container(s); failed to refresh actual state: %v. Retry the operation to finish the refresh.", verb, count, err), http.StatusInternalServerError)
}

func (d *Daemon) handleRollback(w http.ResponseWriter, r *http.Request) {
	var request RollbackRequest
	if !decodeRequest(w, r, &request) {
		return
	}
	if err := agentstate.ValidateIdentity(request.Service, request.Destination); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	d.maintenance.RLock()
	defer d.maintenance.RUnlock()
	lock := d.operationLock(stateKey(request.Service, request.Destination))
	lock.Lock()
	defer lock.Unlock()
	desired, err := d.store.LoadLastGood(request.Service, request.Destination)
	if err != nil {
		http.Error(w, "load last-good state: "+err.Error(), 500)
		return
	}
	var output bytes.Buffer
	result := RollbackResponse{Status: "failed"}
	defer func() { result.Output = output.String(); writeJSON(w, result) }()
	sink := events.NewJSONSink(&output)
	emit := func(name string) error {
		event := healing.LifecycleEvent{Name: name, Service: desired.Service, Destination: desired.Destination, Version: desired.Version, Actor: "serve"}
		if err := sink.Emit(r.Context(), event); err != nil {
			return err
		}
		return d.eventSink.Emit(r.Context(), event)
	}
	if err := emit("rollback_started"); err != nil {
		result.Error = fmt.Sprintf("record rollback_started: %v", err)
		return
	}
	if err := d.commitDesired(r.Context(), desired); err != nil {
		result.Error = err.Error()
		if logErr := emit("rollback_failed"); logErr != nil {
			result.Error += fmt.Sprintf("; record rollback_failed: %v", logErr)
		}
		return
	}
	if err := emit("rollback_completed"); err != nil {
		result.Error = fmt.Sprintf("rollback applied; record rollback_completed: %v", err)
		return
	}
	result.Status = "completed"
	fmt.Fprintf(&output, "Rolled back %s %s to %s\n", desired.Service, desired.Destination, desired.Version)
}
