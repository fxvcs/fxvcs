package rcon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeTransport struct {
	requests  [][]byte
	addresses []string
	response  []byte
	err       error
}

func (f *fakeTransport) RoundTrip(_ context.Context, address string, request []byte, _ time.Duration) ([]byte, error) {
	f.addresses = append(f.addresses, address)
	f.requests = append(f.requests, append([]byte(nil), request...))
	return append([]byte(nil), f.response...), f.err
}

func TestStatusUsesOfficialFiveMOutOfBandFraming(t *testing.T) {
	transport := &fakeTransport{response: []byte("\xff\xff\xff\xffprint players: 2\n")}
	client := Client{Host: "127.0.0.1", Port: 30120, Password: []byte("secret"), Transport: transport}
	status, err := client.Status(context.Background())
	if err != nil || status != "players: 2" {
		t.Fatalf("status=%q err=%v", status, err)
	}
	if got := string(transport.requests[0]); got != "\xff\xff\xff\xffrcon secret status" || transport.addresses[0] != "127.0.0.1:30120" {
		t.Fatalf("request=%q address=%q", got, transport.addresses[0])
	}
}

func TestResourceOperationsAreClosedTypedAndOrdered(t *testing.T) {
	transport := &fakeTransport{response: []byte("\xff\xff\xff\xffprint ok\n")}
	client := Client{Host: "127.0.0.1", Port: 30120, Password: []byte("secret"), Transport: transport}
	operations := []ResourceOperation{{Action: ResourceRefresh}, {Action: ResourceStop, Name: "old-map"}, {Action: ResourceEnsure, Name: "new-map"}, {Action: ResourceRestart, Name: "hud"}}
	if err := client.ApplyResources(context.Background(), operations); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"\xff\xff\xff\xffrcon secret refresh",
		"\xff\xff\xff\xffrcon secret stop old-map",
		"\xff\xff\xff\xffrcon secret ensure new-map",
		"\xff\xff\xff\xffrcon secret restart hud",
	}
	got := make([]string, len(transport.requests))
	for i := range transport.requests {
		got[i] = string(transport.requests[i])
	}
	if !slices.Equal(got, want) {
		t.Fatalf("commands=%q want=%q", got, want)
	}
	for _, bad := range []ResourceOperation{{Action: ResourceEnsure, Name: "bad name"}, {Action: ResourceRestart, Name: "x;quit"}, {Action: "raw", Name: "x"}} {
		if err := client.ApplyResources(context.Background(), []ResourceOperation{bad}); err == nil {
			t.Fatalf("accepted %#v", bad)
		}
	}
}

func TestAuthenticationAndMalformedResponsesAreRedacted(t *testing.T) {
	transport := &fakeTransport{response: []byte("\xff\xff\xff\xffprint Invalid password.\n")}
	client := Client{Host: "127.0.0.1", Port: 30120, Password: []byte("top-secret"), Transport: transport}
	_, err := client.Status(context.Background())
	if !errors.Is(err, ErrAuthentication) || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("authentication error=%v", err)
	}
	transport.response = []byte("not-oob")
	_, err = client.Status(context.Background())
	if err == nil || strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("malformed error=%v", err)
	}
}

func TestClientRefusesUnrepresentableSecretsAndCommands(t *testing.T) {
	for _, password := range []string{"", "has space", "has\nnewline"} {
		client := Client{Host: "127.0.0.1", Port: 30120, Password: []byte(password), Transport: &fakeTransport{}}
		if _, err := client.Status(context.Background()); err == nil {
			t.Fatalf("accepted password %q", password)
		}
	}
}
