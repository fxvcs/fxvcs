package readiness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"fxvcs.dev/fxvcs/internal/domain"
)

// TCPChecker proves that the host-bound server endpoint accepts a connection.
// The portable Target document deliberately carries no physical address.
type TCPChecker struct {
	Dialer  *net.Dialer
	Address string
}

func (c TCPChecker) Validate(domain.HealthCheck) error {
	if c.Address == "" {
		return fmt.Errorf("no host-bound server endpoint")
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return fmt.Errorf("invalid host-bound server endpoint: %w", err)
	}
	return nil
}

func (c TCPChecker) Check(ctx context.Context, _ domain.HealthCheck) Evidence {
	if c.Address == "" {
		return Evidence{Interface: "tcp", Detail: "no host-bound server endpoint"}
	}
	if _, _, err := net.SplitHostPort(c.Address); err != nil {
		return Evidence{Interface: "tcp:" + c.Address, Detail: "invalid host:port endpoint: " + err.Error()}
	}
	dialer := c.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	conn, err := dialer.DialContext(ctx, "tcp", c.Address)
	if err != nil {
		return Evidence{Interface: "tcp:" + c.Address, Detail: err.Error()}
	}
	if err := conn.Close(); err != nil {
		return Evidence{Interface: "tcp:" + c.Address, Detail: err.Error()}
	}
	return Evidence{Interface: "tcp:" + c.Address, Success: true}
}

// HTTPChecker requests every Target-declared FiveM endpoint path against the
// host-bound server address. Redirects are not followed because they would
// change the interface whose health is being asserted.
type HTTPChecker struct {
	Client  *http.Client
	BaseURL string
}

type RCONStatusClient interface {
	Status(context.Context) (string, error)
}

// RCONStatusChecker proves authenticated command execution against the exact
// target-bound RCON endpoint. Console output is deliberately not persisted in
// health evidence because status may contain player identifiers or addresses.
type RCONStatusChecker struct {
	Client RCONStatusClient
}

const maxResourceStateResponseBytes = 64 << 10

// ResourceStateChecker proves authenticated execution of the FXVCS-shipped
// loopback resource. Resource names and states are deliberately not copied
// into durable health evidence.
type ResourceStateChecker struct {
	Client  *http.Client
	BaseURL string
	Token   []byte
}

func (c ResourceStateChecker) Validate(domain.HealthCheck) error {
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme != "http" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return fmt.Errorf("invalid resource-state endpoint")
	}
	host, port, err := net.SplitHostPort(parsed.Host)
	if err != nil || port == "" {
		return fmt.Errorf("invalid resource-state endpoint")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("invalid resource-state endpoint")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("resource-state endpoint is not numeric loopback")
	}
	if len(c.Token) < 16 || len(c.Token) > 512 {
		return fmt.Errorf("resource-state token is missing or invalid")
	}
	for _, char := range c.Token {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("resource-state token is missing or invalid")
		}
	}
	return nil
}

func (c ResourceStateChecker) Check(ctx context.Context, check domain.HealthCheck) Evidence {
	const interfaceName = "fxvcs-resource-state:loopback"
	if err := c.Validate(check); err != nil {
		return Evidence{Interface: interfaceName, Detail: err.Error()}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/fxvcs-health/state", nil)
	if err != nil {
		return Evidence{Interface: interfaceName, Detail: "could not construct bounded request"}
	}
	request.Header.Set("Authorization", "Bearer "+string(c.Token))
	client := c.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	response, err := client.Do(request)
	if err != nil {
		return Evidence{Interface: interfaceName, Detail: "resource-state request failed"}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Evidence{Interface: interfaceName, Detail: fmt.Sprintf("HTTP status %d", response.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResourceStateResponseBytes+1))
	if err != nil || len(data) > maxResourceStateResponseBytes {
		return Evidence{Interface: interfaceName, Detail: "resource-state response exceeded its bound"}
	}
	var report struct {
		Healthy   bool `json:"healthy"`
		Resources []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"resources"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return Evidence{Interface: interfaceName, Detail: "resource-state response was malformed"}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Evidence{Interface: interfaceName, Detail: "resource-state response contained trailing data"}
	}
	if !report.Healthy {
		return Evidence{Interface: interfaceName, Detail: "one or more enabled resources are not started"}
	}
	for _, resource := range report.Resources {
		if resource.Name == "" || resource.State != "started" {
			return Evidence{Interface: interfaceName, Detail: "resource-state report disagrees with healthy status"}
		}
	}
	return Evidence{Interface: interfaceName, Success: true}
}

func (c RCONStatusChecker) Validate(domain.HealthCheck) error {
	if c.Client == nil {
		return fmt.Errorf("no credentialed RCON client")
	}
	return nil
}

func (c RCONStatusChecker) Check(ctx context.Context, _ domain.HealthCheck) Evidence {
	if c.Client == nil {
		return Evidence{Interface: "rcon-status", Detail: "no credentialed RCON client"}
	}
	status, err := c.Client.Status(ctx)
	if err != nil {
		return Evidence{Interface: "rcon-status", Detail: err.Error()}
	}
	if strings.TrimSpace(status) == "" {
		return Evidence{Interface: "rcon-status", Detail: "authenticated status returned no evidence"}
	}
	return Evidence{Interface: "rcon-status", Success: true}
}

func (c HTTPChecker) Validate(check domain.HealthCheck) error {
	if c.BaseURL == "" {
		return fmt.Errorf("no host-bound server endpoint")
	}
	if len(check.Endpoints) == 0 {
		return fmt.Errorf("no endpoint paths declared")
	}
	return nil
}

func (c HTTPChecker) Check(ctx context.Context, check domain.HealthCheck) Evidence {
	if c.BaseURL == "" {
		return Evidence{Interface: check.Type, Detail: "no host-bound server endpoint"}
	}
	if len(check.Endpoints) == 0 {
		return Evidence{Interface: check.Type, Detail: "no endpoint paths declared"}
	}
	client := c.Client
	if client == nil {
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	for _, endpoint := range check.Endpoints {
		if !strings.HasPrefix(endpoint, "/") || strings.HasPrefix(endpoint, "//") {
			return Evidence{Interface: check.Type + ":" + endpoint, Detail: "invalid absolute endpoint path"}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+endpoint, nil)
		if err != nil {
			return Evidence{Interface: check.Type + ":" + endpoint, Detail: err.Error()}
		}
		resp, err := client.Do(req)
		if err != nil {
			return Evidence{Interface: check.Type + ":" + endpoint, Detail: err.Error()}
		}
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return Evidence{Interface: check.Type + ":" + endpoint, Detail: fmt.Sprintf("HTTP status %d", resp.StatusCode)}
		}
	}
	return Evidence{Interface: check.Type + ":" + strings.Join(check.Endpoints, ","), Success: true}
}

// PortableCheckers binds portable TCP/HTTP checks to an agent-local server
// address. Credentialed checks are added only after secret resolution.
func PortableCheckers(endpoint *domain.RCONEndpointRef) map[string]Checker {
	address, baseURL := "", ""
	if endpoint != nil && endpoint.Host != "" && endpoint.Port > 0 {
		address = net.JoinHostPort(endpoint.Host, strconv.Itoa(endpoint.Port))
		baseURL = "http://" + address
	}
	return map[string]Checker{
		"tcp":        TCPChecker{Address: address},
		"fivem-http": HTTPChecker{BaseURL: baseURL},
	}
}
