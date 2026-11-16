package health

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type Status string

const (
	Healthy   Status = "healthy"
	Unhealthy Status = "unhealthy"
)

type Target struct {
	ContainerName string
	Address       string
	Port          int
	Path          string
}

type Checker interface {
	Check(ctx context.Context, target Target) (Status, error)
}

type HTTPChecker struct {
	client *http.Client
}

// DefaultProbeTimeout bounds a single health probe when the caller supplies
// no client. Probes run while the agent holds reconcile and service locks,
// so a workload must never be able to stall them indefinitely.
const DefaultProbeTimeout = 5 * time.Second

func NewHTTPChecker(client *http.Client) *HTTPChecker {
	if client == nil {
		client = NewHTTPClient(DefaultProbeTimeout)
	}
	return &HTTPChecker{client: client}
}

// NewHTTPClient returns a client suitable for probing untrusted workloads
// from the root agent: it is time-bounded, ignores proxy environment
// variables, and never follows redirects (a 3xx already counts as healthy,
// and following one would let a workload aim the agent at host-local
// services).
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (c *HTTPChecker) Check(ctx context.Context, target Target) (Status, error) {
	url := target.Address
	if strings.TrimSpace(url) == "" {
		return Unhealthy, nil
	}
	path := target.Path
	if path == "" {
		path = "/"
	}
	url = strings.TrimRight(url, "/") + "/" + strings.TrimLeft(path, "/")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Unhealthy, err
	}
	response, err := c.client.Do(request)
	if err != nil {
		return Unhealthy, nil
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 400 {
		return Healthy, nil
	}
	return Unhealthy, nil
}
