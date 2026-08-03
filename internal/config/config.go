package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Service          string                      `yaml:"service"`
	Image            string                      `yaml:"image"`
	Destination      string                      `yaml:"destination"`
	Builder          BuilderConfig               `yaml:"builder"`
	Servers          map[string]ServerConfig     `yaml:"servers"`
	Proxy            ProxyConfig                 `yaml:"proxy"`
	Networking       NetworkingConfig            `yaml:"networking"`
	RetainContainers int                         `yaml:"retain_containers"`
	Dependencies     map[string]DependencyConfig `yaml:"dependencies"`
	Accessories      map[string]DependencyConfig `yaml:"accessories"`
	Env              EnvConfig                   `yaml:"env"`
	Secrets          SecretsConfig               `yaml:"secrets"`
	Observability    ObservabilityConfig         `yaml:"observability"`
	dependencyField  string
}

type BuilderConfig struct {
	Context    string `yaml:"context"`
	Dockerfile string `yaml:"dockerfile"`
	Arch       string `yaml:"arch"`
}

type ServerConfig struct {
	Hosts       []string          `yaml:"hosts"`
	Aliases     []string          `yaml:"aliases"`
	Command     string            `yaml:"command"`
	AppPort     int               `yaml:"app_port"`
	Replicas    int               `yaml:"replicas"`
	Healthcheck HealthcheckConfig `yaml:"healthcheck"`
	Restart     RestartConfig     `yaml:"restart"`
}

type HealthcheckConfig struct {
	HTTP     HTTPHealthcheckConfig `yaml:"http"`
	Interval Duration              `yaml:"interval"`
	Timeout  Duration              `yaml:"timeout"`
	Retries  int                   `yaml:"retries"`
}

type HTTPHealthcheckConfig struct {
	Path string `yaml:"path"`
	Port int    `yaml:"port"`
}

type RestartConfig struct {
	Policy         string   `yaml:"policy"`
	Controller     string   `yaml:"controller"`
	InitialBackoff Duration `yaml:"initial_backoff"`
	MaxBackoff     Duration `yaml:"max_backoff"`
	MaxAttempts    int      `yaml:"max_attempts"`
	Window         Duration `yaml:"window"`
}

type ProxyConfig struct {
	Provider      string   `yaml:"provider"`
	Hosts         []string `yaml:"hosts"`
	AppRole       string   `yaml:"app_role"`
	SSL           string   `yaml:"ssl"`
	DeployTimeout Duration `yaml:"deploy_timeout"`
	DrainTimeout  Duration `yaml:"drain_timeout"`
}

type NetworkingConfig struct {
	PrivateNetwork string `yaml:"private_network"`
}

type DependencyConfig struct {
	Image        string        `yaml:"image"`
	Hosts        []string      `yaml:"hosts"`
	Aliases      []string      `yaml:"aliases"`
	InternalPort int           `yaml:"internal_port"`
	Volumes      []string      `yaml:"volumes"`
	Restart      RestartConfig `yaml:"restart"`
	Env          EnvConfig     `yaml:"env"`
}

// AccessoryConfig is kept as a source-compatible alias while accessories is
// accepted as the deprecated YAML name for dependencies.
type AccessoryConfig = DependencyConfig

type EnvConfig struct {
	Plain  map[string]string `yaml:"plain"`
	Secret []string          `yaml:"secret"`
}

type SecretsConfig struct {
	Provider string `yaml:"provider"`
	KMS      string `yaml:"kms"`
	Key      string `yaml:"key"`
}

func HasEnvSecrets(cfg Config) bool {
	if len(cfg.Env.Secret) > 0 {
		return true
	}
	dependencies := cfg.Dependencies
	if dependencies == nil {
		dependencies = cfg.Accessories
	}
	for _, accessory := range dependencies {
		if len(accessory.Env.Secret) > 0 {
			return true
		}
	}
	return false
}

type ObservabilityConfig struct {
	Logs          LogConfig           `yaml:"logs"`
	Metrics       MetricsConfig       `yaml:"metrics"`
	RuntimeEvents RuntimeEventsConfig `yaml:"runtime_events"`
}

type LogConfig struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"`
}

type RuntimeEventsConfig struct {
	Enabled          bool `yaml:"enabled"`
	LogRestarts      bool `yaml:"log_restarts"`
	LogHealthChanges bool `yaml:"log_health_changes"`
	LogOOM           bool `yaml:"log_oom"`
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var sshHostPattern = regexp.MustCompile(`^[A-Za-z0-9\[][A-Za-z0-9_.:@\[\]%-]*$`)

type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("duration must be a scalar")
	}
	if value.Value == "" {
		d.Duration = 0
		return nil
	}

	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = parsed
	return nil
}

type LoadOption func(*loadOptions)

type loadOptions struct {
	destination string
}

func WithDestination(destination string) LoadOption {
	return func(options *loadOptions) {
		options.destination = destination
	}
}

func Load(path string, opts ...LoadOption) (Config, error) {
	merged, err := loadMergedMap(path, opts...)
	if err != nil {
		return Config{}, err
	}
	return decodeConfig(path, merged)
}

// LoadServices loads either a legacy single-service config or a multi-service
// manifest and returns normalized service configs in deterministic name order.
func LoadServices(path string, opts ...LoadOption) ([]Config, error) {
	merged, err := loadMergedMap(path, opts...)
	if err != nil {
		return nil, err
	}

	rawServices, ok := merged["services"]
	if !ok {
		cfg, err := decodeConfig(path, merged)
		if err != nil {
			return nil, err
		}
		return []Config{cfg}, nil
	}
	for _, key := range []string{"service", "image", "builder", "servers", "proxy", "dependencies", "accessories", "env", "secrets", "observability"} {
		if _, configured := merged[key]; configured {
			return nil, fmt.Errorf("invalid config: %s cannot be configured beside services", key)
		}
	}
	allowed := map[string]bool{"services": true, "destination": true, "networking": true, "retain_containers": true}
	for key := range merged {
		if !allowed[key] {
			return nil, fmt.Errorf("parse config %q: field %s not found", path, key)
		}
	}

	services, ok := rawServices.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parse config %q: services must be a map", path)
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("invalid config: at least one service is required")
	}

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	configs := make([]Config, 0, len(names))
	for _, name := range names {
		service, ok := services[name].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("parse config %q: services.%s must be a map", path, name)
		}
		for _, key := range []string{"service", "destination", "networking", "retain_containers"} {
			if _, configured := service[key]; configured {
				return nil, fmt.Errorf("invalid config: services.%s.%s must be configured at the top level", name, key)
			}
		}
		normalized := map[string]any{"service": name}
		for _, key := range []string{"destination", "networking", "retain_containers"} {
			if value, ok := merged[key]; ok {
				normalized[key] = value
			}
		}
		for key, value := range service {
			normalized[key] = value
		}
		cfg, err := decodeConfig(path, normalized)
		if err != nil {
			return nil, err
		}
		configs = append(configs, cfg)
	}

	proxyHosts := map[string]string{}
	for _, cfg := range configs {
		for _, host := range cfg.Proxy.Hosts {
			if owner, exists := proxyHosts[host]; exists {
				return nil, fmt.Errorf("invalid config: proxy host %q is configured by both %s and %s", host, owner, cfg.Service)
			}
			proxyHosts[host] = cfg.Service
		}
	}
	return configs, nil
}

func loadMergedMap(path string, opts ...LoadOption) (map[string]any, error) {
	options := loadOptions{}
	for _, opt := range opts {
		opt(&options)
	}

	baseBytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("config file not found: %s", path)
	}
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	merged, err := decodeMap(path, baseBytes)
	if err != nil {
		return nil, err
	}

	destination := options.destination
	if destination == "" {
		destination, _ = merged["destination"].(string)
	}
	if destination != "" {
		overlayPath := overlayPath(path, destination)
		overlayBytes, err := os.ReadFile(overlayPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("read destination overlay %q: %w", overlayPath, err)
		}
		if err == nil {
			overlay, err := decodeMap(overlayPath, overlayBytes)
			if err != nil {
				return nil, err
			}
			merged = deepMerge(merged, overlay)
		}
	}
	return merged, nil
}

func decodeConfig(path string, merged map[string]any) (Config, error) {
	_, hasDependencies := merged["dependencies"]
	_, hasAccessories := merged["accessories"]
	if hasDependencies && hasAccessories {
		return Config{}, fmt.Errorf("invalid config: dependencies and accessories cannot both be configured")
	}

	mergedBytes, err := yaml.Marshal(merged)
	if err != nil {
		return Config{}, fmt.Errorf("encode merged config: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(mergedBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", path, err)
	}

	if hasAccessories {
		cfg.dependencyField = "accessories"
	} else {
		cfg.dependencyField = "dependencies"
	}
	if cfg.Dependencies == nil {
		cfg.Dependencies = cfg.Accessories
	}
	if cfg.Accessories == nil {
		cfg.Accessories = cfg.Dependencies
	}

	applyDefaults(&cfg)
	if err := validate(cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeMap(path string, data []byte) (map[string]any, error) {
	decoded := map[string]any{}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	return decoded, nil
}

func overlayPath(path string, destination string) string {
	extension := filepath.Ext(path)
	name := strings.TrimSuffix(filepath.Base(path), extension)
	return filepath.Join(filepath.Dir(path), name+"."+destination+extension)
}

func deepMerge(base map[string]any, overlay map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(overlay))
	for key, value := range base {
		merged[key] = value
	}

	for key, overlayValue := range overlay {
		baseMap, baseOK := merged[key].(map[string]any)
		overlayMap, overlayOK := overlayValue.(map[string]any)
		if baseOK && overlayOK {
			merged[key] = deepMerge(baseMap, overlayMap)
			continue
		}
		merged[key] = overlayValue
	}

	return merged
}

func applyDefaults(cfg *Config) {
	if cfg.Networking.PrivateNetwork == "" {
		cfg.Networking.PrivateNetwork = "serve"
	}
	if cfg.RetainContainers == 0 {
		cfg.RetainContainers = 5
	}

	for name, server := range cfg.Servers {
		applyRestartDefaults(&server.Restart)
		cfg.Servers[name] = server
	}
	for name, accessory := range cfg.Accessories {
		applyRestartDefaults(&accessory.Restart)
		cfg.Accessories[name] = accessory
	}
}

func applyRestartDefaults(restart *RestartConfig) {
	if restart.Controller == "" {
		restart.Controller = "agent"
	}
}

func validate(cfg Config) error {
	var problems []string
	if strings.TrimSpace(cfg.Service) == "" {
		problems = append(problems, "service is required")
	} else if !validIdentifier(cfg.Service) {
		problems = append(problems, "service must contain only letters, numbers, dots, underscores, and hyphens")
	}
	if strings.TrimSpace(cfg.Image) == "" {
		problems = append(problems, "image is required")
	}
	if cfg.Destination != "" && !validIdentifier(cfg.Destination) {
		problems = append(problems, "destination must contain only letters, numbers, dots, underscores, and hyphens")
	}
	if cfg.Networking.PrivateNetwork != "" && !validIdentifier(cfg.Networking.PrivateNetwork) {
		problems = append(problems, "networking.private_network must be a valid identifier")
	}
	if cfg.RetainContainers < 0 {
		problems = append(problems, "retain_containers must not be negative")
	}

	aliasOwners := map[string]string{}
	serverRoles := make([]string, 0, len(cfg.Servers))
	for role := range cfg.Servers {
		serverRoles = append(serverRoles, role)
	}
	sort.Strings(serverRoles)
	for _, role := range serverRoles {
		problems = append(problems, validateAliases("servers."+role+".aliases", cfg.Servers[role].Aliases, aliasOwners)...)
	}
	accessoryNames := make([]string, 0, len(cfg.Accessories))
	for name := range cfg.Accessories {
		accessoryNames = append(accessoryNames, name)
	}
	sort.Strings(accessoryNames)
	for _, name := range accessoryNames {
		problems = append(problems, validateAliases("accessories."+name+".aliases", cfg.Accessories[name].Aliases, aliasOwners)...)
	}

	for role, server := range cfg.Servers {
		if !validIdentifier(role) {
			problems = append(problems, fmt.Sprintf("servers.%s must use a valid identifier", role))
		}
		problems = append(problems, validateRestart("servers."+role+".restart", server.Restart)...)
		problems = append(problems, validateHealthcheck("servers."+role+".healthcheck", server.Healthcheck)...)
		problems = append(problems, validateSSHHosts("servers."+role+".hosts", server.Hosts)...)
		if server.Replicas < 0 {
			problems = append(problems, "servers."+role+".replicas must not be negative")
		}
	}
	dependencies := cfg.Dependencies
	if dependencies == nil {
		dependencies = cfg.Accessories
	}
	dependencyField := cfg.dependencyField
	if dependencyField == "" {
		dependencyField = "dependencies"
	}
	for name, dependency := range dependencies {
		path := dependencyField + "." + name
		if !validIdentifier(name) {
			problems = append(problems, fmt.Sprintf("%s must use a valid identifier", path))
		}
		if _, exists := cfg.Servers[name]; exists {
			problems = append(problems, fmt.Sprintf("%s cannot be both a server role and a dependency", name))
		}
		problems = append(problems, validateRestart(path+".restart", dependency.Restart)...)
		problems = append(problems, validateSSHHosts(path+".hosts", dependency.Hosts)...)
		if strings.TrimSpace(dependency.Image) == "" {
			problems = append(problems, path+".image is required")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid config: %s", strings.Join(problems, "; "))
	}
	return nil
}

func ValidateSSHHost(host string) error {
	if !sshHostPattern.MatchString(host) {
		return fmt.Errorf("invalid SSH host %q", host)
	}
	return nil
}

func validateSSHHosts(path string, hosts []string) []string {
	var problems []string
	for _, host := range hosts {
		if err := ValidateSSHHost(host); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
		}
	}
	return problems
}

func validIdentifier(value string) bool {
	return identifierPattern.MatchString(value)
}

func validateAliases(path string, aliases []string, owners map[string]string) []string {
	var problems []string
	for _, alias := range aliases {
		if !validIdentifier(alias) {
			problems = append(problems, path+" must contain only valid identifiers")
			continue
		}
		if owner, exists := owners[alias]; exists {
			problems = append(problems, fmt.Sprintf("%s: alias %q is also used by %s", path, alias, owner))
			continue
		}
		owners[alias] = path[:strings.LastIndex(path, ".aliases")]
	}
	return problems
}

func validateHealthcheck(path string, check HealthcheckConfig) []string {
	var problems []string
	if check.Interval.Duration < 0 {
		problems = append(problems, path+".interval must be positive")
	}
	if check.Timeout.Duration < 0 {
		problems = append(problems, path+".timeout must be positive")
	}
	if check.Retries < 0 {
		problems = append(problems, path+".retries must not be negative")
	}
	return problems
}

func validateRestart(path string, restart RestartConfig) []string {
	var problems []string
	if !validRestartPolicy(restart.Policy) {
		problems = append(problems, fmt.Sprintf("%s.policy must be one of always, unless-stopped, on-failure, no", path))
	}
	if !validRestartController(restart.Controller) {
		problems = append(problems, fmt.Sprintf("%s.controller must be one of agent, docker", path))
	}
	if restart.InitialBackoff.Duration < 0 {
		problems = append(problems, path+".initial_backoff must be positive")
	}
	if restart.MaxBackoff.Duration < 0 {
		problems = append(problems, path+".max_backoff must be positive")
	}
	if restart.MaxAttempts < 0 {
		problems = append(problems, path+".max_attempts must not be negative")
	}
	if restart.Window.Duration < 0 {
		problems = append(problems, path+".window must be positive")
	}
	if restart.Controller == "docker" {
		if restart.InitialBackoff.Duration > 0 {
			problems = append(problems, path+".initial_backoff is only supported by the agent restart controller")
		}
		if restart.MaxBackoff.Duration > 0 {
			problems = append(problems, path+".max_backoff is only supported by the agent restart controller")
		}
		if restart.Window.Duration > 0 {
			problems = append(problems, path+".window is only supported by the agent restart controller")
		}
		if restart.MaxAttempts > 0 && restart.Policy != "on-failure" {
			problems = append(problems, path+".max_attempts requires policy on-failure with the docker restart controller")
		}
	}
	return problems
}

func validRestartPolicy(policy string) bool {
	switch policy {
	case "", "always", "unless-stopped", "on-failure", "no":
		return true
	default:
		return false
	}
}

func validRestartController(controller string) bool {
	switch controller {
	case "agent", "docker":
		return true
	default:
		return false
	}
}
