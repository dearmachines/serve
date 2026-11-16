package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/uptimenine/serve/internal/planner"
)

// Store keeps per-identity state under dir/<service>/<destination>.<kind>.json.
// Identities may contain dots, so the service is a directory and the kind is
// a fixed suffix; no two (service, destination) pairs share a path.
type Store struct {
	dir string

	migrateOnce sync.Once
	migrateErr  error
}

var stateKinds = []string{"desired", "actual", "last-good"}

var identityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

type ActualState struct {
	Service     string            `json:"service"`
	Destination string            `json:"destination"`
	Containers  []ActualContainer `json:"containers"`
}

type ActualContainer struct {
	Name    string `json:"name"`
	Role    string `json:"role"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

func NewStore(dir string) *Store {
	return &Store{dir: dir}
}

func (s *Store) Dir() string {
	return s.dir
}

// Path returns the file that holds one kind of state for an identity.
func (s *Store) Path(service string, destination string, kind string) string {
	return statePath(s.dir, service, destination, kind)
}

// migrate moves flat files written by older agents
// ("<service>.<destination>.<kind>.json", ambiguous for dotted names) to the
// per-service layout. The identity comes from the file's own contents.
func (s *Store) migrate() error {
	s.migrateOnce.Do(func() { s.migrateErr = migrateLegacy(s.dir) })
	return s.migrateErr
}

func migrateLegacy(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state directory: %w", err)
	}
	var errs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		for _, kind := range stateKinds {
			if !strings.HasSuffix(entry.Name(), "."+kind+".json") {
				continue
			}
			legacy := filepath.Join(dir, entry.Name())
			var identity struct {
				Service     string `json:"service"`
				Destination string `json:"destination"`
			}
			if err := loadJSON(legacy, &identity); err != nil {
				errs = append(errs, fmt.Errorf("migrate %q: %w", entry.Name(), err))
				continue
			}
			if err := ValidateIdentity(identity.Service, identity.Destination); err != nil {
				errs = append(errs, fmt.Errorf("migrate %q: %w", entry.Name(), err))
				continue
			}
			target := statePath(dir, identity.Service, identity.Destination, kind)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				errs = append(errs, err)
				continue
			}
			if _, err := os.Stat(target); err == nil {
				// Already migrated; the new layout wins over a stale copy.
				errs = append(errs, os.Remove(legacy))
				continue
			}
			errs = append(errs, os.Rename(legacy, target))
		}
	}
	return errors.Join(errs...)
}

// Forget removes deployment state without touching volume data. Delete desired
// last so a cleanup failure cannot leave the daemon healing a forgotten service.
func (s *Store) Forget(service, destination string) error {
	if err := s.migrate(); err != nil {
		return err
	}
	if err := ValidateIdentity(service, destination); err != nil {
		return err
	}
	for _, kind := range []string{"last-good", "actual", "desired"} {
		if err := os.Remove(statePath(s.dir, service, destination, kind)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) SaveDesired(desired planner.DesiredState) error {
	if err := s.migrate(); err != nil {
		return err
	}
	if err := ValidateIdentity(desired.Service, desired.Destination); err != nil {
		return err
	}
	return s.saveJSON(statePath(s.dir, desired.Service, desired.Destination, "desired"), desired)
}

func (s *Store) LoadDesired(service string, destination string) (planner.DesiredState, error) {
	if err := s.migrate(); err != nil {
		return planner.DesiredState{}, err
	}
	if err := ValidateIdentity(service, destination); err != nil {
		return planner.DesiredState{}, err
	}
	var desired planner.DesiredState
	if err := loadJSON(statePath(s.dir, service, destination, "desired"), &desired); err != nil {
		return planner.DesiredState{}, err
	}
	return desired, nil
}

// ListDesired loads every desired state saved in the store. A file that
// cannot be read is reported in the error, but the valid states are still
// returned so one bad file does not stop reconcile for every service.
func (s *Store) ListDesired() ([]planner.DesiredState, error) {
	var errs []error
	if err := s.migrate(); err != nil {
		errs = append(errs, err)
	}
	services, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.Join(errs...)
		}
		return nil, fmt.Errorf("read state directory: %w", err)
	}

	var states []planner.DesiredState
	for _, service := range services {
		if !service.IsDir() {
			continue
		}
		serviceDir := filepath.Join(s.dir, service.Name())
		entries, err := os.ReadDir(serviceDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read state directory %q: %w", service.Name(), err))
			continue
		}
		for _, entry := range entries {
			destination, ok := strings.CutSuffix(entry.Name(), ".desired.json")
			if entry.IsDir() || !ok {
				continue
			}
			var desired planner.DesiredState
			if err := loadJSON(filepath.Join(serviceDir, entry.Name()), &desired); err != nil {
				errs = append(errs, err)
				continue
			}
			if desired.Service != service.Name() || desired.Destination != destination {
				errs = append(errs, fmt.Errorf("desired state %q records identity %s/%s", filepath.Join(service.Name(), entry.Name()), desired.Service, desired.Destination))
				continue
			}
			if err := ValidateIdentity(desired.Service, desired.Destination); err != nil {
				errs = append(errs, fmt.Errorf("invalid desired state %q: %w", entry.Name(), err))
				continue
			}
			states = append(states, desired)
		}
	}
	return states, errors.Join(errs...)
}

func (s *Store) SaveActual(actual ActualState) error {
	if err := s.migrate(); err != nil {
		return err
	}
	if err := ValidateIdentity(actual.Service, actual.Destination); err != nil {
		return err
	}
	return s.saveJSON(statePath(s.dir, actual.Service, actual.Destination, "actual"), actual)
}

func (s *Store) LoadActual(service string, destination string) (ActualState, error) {
	if err := s.migrate(); err != nil {
		return ActualState{}, err
	}
	if err := ValidateIdentity(service, destination); err != nil {
		return ActualState{}, err
	}
	var actual ActualState
	path := statePath(s.dir, service, destination, "actual")
	if err := loadJSON(path, &actual); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ActualState{Service: service, Destination: destination}, nil
		}
		return ActualState{}, err
	}
	return actual, nil
}

func (s *Store) SaveLastGood(desired planner.DesiredState) error {
	if err := s.migrate(); err != nil {
		return err
	}
	if err := ValidateIdentity(desired.Service, desired.Destination); err != nil {
		return err
	}
	return s.saveJSON(statePath(s.dir, desired.Service, desired.Destination, "last-good"), desired)
}

func (s *Store) LoadLastGood(service string, destination string) (planner.DesiredState, error) {
	if err := s.migrate(); err != nil {
		return planner.DesiredState{}, err
	}
	if err := ValidateIdentity(service, destination); err != nil {
		return planner.DesiredState{}, err
	}
	var desired planner.DesiredState
	if err := loadJSON(statePath(s.dir, service, destination, "last-good"), &desired); err != nil {
		return planner.DesiredState{}, err
	}
	return desired, nil
}

func (s *Store) saveJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary state file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	encoder := json.NewEncoder(tmp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		tmp.Close()
		return fmt.Errorf("encode state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temporary state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state file: %w", err)
	}
	// Persist the rename itself so a crash cannot roll the file back.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		syncErr := dir.Sync()
		dir.Close()
		if syncErr != nil {
			return fmt.Errorf("sync state directory: %w", syncErr)
		}
	}

	return nil
}

func loadJSON(path string, target any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := json.NewDecoder(file).Decode(target); err != nil {
		return fmt.Errorf("decode state file %q: %w", path, err)
	}
	return nil
}

func statePath(dir string, service string, destination string, kind string) string {
	return filepath.Join(dir, service, destination+"."+kind+".json")
}

func ValidateIdentity(service string, destination string) error {
	if !identityPattern.MatchString(service) {
		return fmt.Errorf("invalid service identifier %q", service)
	}
	if !identityPattern.MatchString(destination) {
		return fmt.Errorf("invalid destination identifier %q", destination)
	}
	return nil
}
