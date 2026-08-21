package readiness

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
)

type statusClient struct {
	status string
	err    error
}

func (c statusClient) Status(context.Context) (string, error) { return c.status, c.err }

func TestRCONStatusCheckerPersistsNoConsoleOutput(t *testing.T) {
	checker := RCONStatusChecker{Client: statusClient{status: "player secret-address"}}
	evidence := checker.Check(context.Background(), domain.HealthCheck{Type: "rcon-status"})
	if !evidence.Success || evidence.Interface != "rcon-status" || evidence.Detail != "" {
		t.Fatalf("evidence=%#v", evidence)
	}
	checker.Client = statusClient{err: errors.New("redacted failure")}
	evidence = checker.Check(context.Background(), domain.HealthCheck{Type: "rcon-status"})
	if evidence.Success || evidence.Detail != "redacted failure" {
		t.Fatalf("failure evidence=%#v", evidence)
	}
}

func TestTCPCheckerUsesOnlyHostBoundEndpoint(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		conn, _ := listener.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		close(done)
	}()
	checker := TCPChecker{Address: listener.Addr().String()}
	if err := checker.Validate(domain.HealthCheck{Type: "tcp"}); err != nil {
		t.Fatal(err)
	}
	evidence := checker.Check(context.Background(), domain.HealthCheck{Type: "tcp"})
	if !evidence.Success || evidence.Interface == "" {
		t.Fatalf("unexpected evidence: %#v", evidence)
	}
	<-done
}

func TestHTTPCheckerAppendsPortablePathsAndRefusesRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info.json", "/dynamic.json":
			w.WriteHeader(http.StatusOK)
		case "/redirect":
			http.Redirect(w, r, "/info.json", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	checker := HTTPChecker{BaseURL: server.URL}
	check := domain.HealthCheck{Type: "fivem-http", Endpoints: []string{"/info.json", "/dynamic.json"}}
	if err := checker.Validate(check); err != nil {
		t.Fatal(err)
	}
	if evidence := checker.Check(context.Background(), check); !evidence.Success {
		t.Fatalf("healthy endpoints failed: %#v", evidence)
	}
	if evidence := checker.Check(context.Background(), domain.HealthCheck{Type: "fivem-http", Endpoints: []string{"/redirect"}}); evidence.Success {
		t.Fatalf("redirect was followed: %#v", evidence)
	}
}

func TestResourceStateCheckerRequiresLoopbackAuthAndStrictHealthyReport(t *testing.T) {
	token := []byte("0123456789abcdef0123456789abcdef")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fxvcs-health/state" || r.Header.Get("Authorization") != "Bearer "+string(token) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"healthy":true,"resources":[{"name":"chat","state":"started"}]}`))
	}))
	defer server.Close()
	checker := ResourceStateChecker{BaseURL: server.URL, Token: token}
	check := domain.HealthCheck{Type: "fxvcs-resource-state"}
	if err := checker.Validate(check); err != nil {
		t.Fatal(err)
	}
	if evidence := checker.Check(context.Background(), check); !evidence.Success || evidence.Detail != "" {
		t.Fatalf("healthy report failed: %#v", evidence)
	}
	checker.BaseURL = "http://192.0.2.1:30120"
	if err := checker.Validate(check); err == nil {
		t.Fatal("non-loopback resource-state endpoint accepted")
	}
	for _, invalid := range []string{"http://127.0.0.1:not-a-port", "http://127.0.0.1:30120#fragment"} {
		checker.BaseURL = invalid
		if err := checker.Validate(check); err == nil {
			t.Fatalf("invalid resource-state endpoint %q accepted", invalid)
		}
	}
}

func TestResourceStateCheckerFailsClosedWithoutPersistingReport(t *testing.T) {
	token := []byte("0123456789abcdef")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"healthy":false,"resources":[{"name":"private-resource","state":"stopped"}]}`))
	}))
	defer server.Close()
	evidence := (ResourceStateChecker{BaseURL: server.URL, Token: token}).Check(context.Background(), domain.HealthCheck{Type: "fxvcs-resource-state"})
	if evidence.Success || strings.Contains(evidence.Detail, "private-resource") || strings.Contains(evidence.Detail, "stopped") {
		t.Fatalf("unsafe failure evidence: %#v", evidence)
	}
}

func TestResourceStateCheckerRefusesRedirectOversizeAndTrailingJSON(t *testing.T) {
	token := []byte("0123456789abcdef")
	mode := "redirect"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "redirect":
			http.Redirect(w, r, "/fxvcs-health/state", http.StatusFound)
		case "oversize":
			_, _ = w.Write(bytes.Repeat([]byte("x"), maxResourceStateResponseBytes+1))
		case "trailing":
			_, _ = w.Write([]byte(`{"healthy":true,"resources":[]} {}`))
		}
	}))
	defer server.Close()
	checker := ResourceStateChecker{BaseURL: server.URL, Token: token}
	for _, testMode := range []string{"redirect", "oversize", "trailing"} {
		mode = testMode
		if evidence := checker.Check(context.Background(), domain.HealthCheck{Type: "fxvcs-resource-state"}); evidence.Success {
			t.Fatalf("%s response accepted: %#v", testMode, evidence)
		}
	}
}
