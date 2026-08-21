package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"fxvcs.dev/fxvcs/internal/domain"
)

const defaultFiveMHTTPObservationLimit int64 = 64 << 10

// PlayerCountObserver obtains a live player count without lifecycle authority.
type PlayerCountObserver interface {
	ObservePlayerCount(context.Context) (int, error)
}

// FiveMHTTPPlayerCount reads the non-credentialed /dynamic.json endpoint bound
// by the target administrator. It neither discovers nor follows another host.
type FiveMHTTPPlayerCount struct {
	Endpoint     *domain.RCONEndpointRef
	Client       *http.Client
	MaxBodyBytes int64
}

func (o FiveMHTTPPlayerCount) ObservePlayerCount(ctx context.Context) (int, error) {
	if o.Endpoint == nil || o.Endpoint.Host == "" || o.Endpoint.Port < 1 || o.Endpoint.Port > 65535 {
		return 0, errors.New("supervisor: FiveM HTTP player observation requires a bound host and port")
	}
	client := o.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &clientCopy
	limit := o.MaxBodyBytes
	if limit <= 0 {
		limit = defaultFiveMHTTPObservationLimit
	}
	endpoint := url.URL{Scheme: "http", Host: net.JoinHostPort(o.Endpoint.Host, strconv.Itoa(o.Endpoint.Port)), Path: "/dynamic.json"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("supervisor: build FiveM player observation: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("supervisor: observe FiveM players: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("supervisor: observe FiveM players: unexpected HTTP status %d", resp.StatusCode)
	}
	var payload struct {
		Clients *int `json:"clients"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, fmt.Errorf("supervisor: read FiveM player observation: %w", err)
	}
	if int64(len(body)) > limit {
		return 0, errors.New("supervisor: FiveM player observation exceeded the bounded response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		return 0, fmt.Errorf("supervisor: decode FiveM player observation: %w", err)
	}
	if payload.Clients == nil || *payload.Clients < 0 {
		return 0, errors.New("supervisor: FiveM player observation omitted a valid clients count")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return 0, errors.New("supervisor: FiveM player observation contained trailing JSON")
		}
		return 0, fmt.Errorf("supervisor: FiveM player observation exceeded the bounded response: %w", err)
	}
	return *payload.Clients, nil
}
