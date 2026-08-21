package supervisor

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func playerHTTPClient(t *testing.T, code int, body string) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "http://127.0.0.1:30120/dynamic.json" {
			t.Fatalf("endpoint=%q", req.URL.String())
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: req}, nil
	})}
}

func TestFiveMHTTPPlayerCountUsesBoundDynamicEndpoint(t *testing.T) {
	count, err := (FiveMHTTPPlayerCount{
		Endpoint: &domain.RCONEndpointRef{Host: "127.0.0.1", Port: 30120},
		Client:   playerHTTPClient(t, http.StatusOK, `{"clients":7,"sv_maxclients":48}`),
	}).ObservePlayerCount(context.Background())
	if err != nil || count != 7 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestFiveMHTTPPlayerCountRefusesRedirectMissingAndOversize(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want string
	}{
		{name: "redirect", code: http.StatusFound, want: "status 302"},
		{name: "missing", code: http.StatusOK, body: `{}`, want: "omitted"},
		{name: "negative", code: http.StatusOK, body: `{"clients":-1}`, want: "omitted"},
		{name: "oversize", code: http.StatusOK, body: `{"clients":1}` + strings.Repeat(" ", 128), want: "bounded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (FiveMHTTPPlayerCount{
				Endpoint:     &domain.RCONEndpointRef{Host: "127.0.0.1", Port: 30120},
				Client:       playerHTTPClient(t, tc.code, tc.body),
				MaxBodyBytes: 32,
			}).ObservePlayerCount(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want substring %q", err, tc.want)
			}
		})
	}
}
