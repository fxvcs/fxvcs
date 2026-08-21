// Package rcon implements the FiveM UDP out-of-band RCON protocol exposed by
// the official citizen-server implementation.
package rcon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

var (
	ErrAuthentication = errors.New("rcon: authentication failed")
	ErrUnavailable    = errors.New("rcon: server unavailable")
)

const (
	requestPrefix  = "\xff\xff\xff\xffrcon "
	responsePrefix = "\xff\xff\xff\xffprint "
	maxDatagram    = 65507
	maxCommand     = 1024
	defaultTimeout = 5 * time.Second
)

// Transport exchanges one exact request/response datagram with the bound
// endpoint. Implementations must not accept responses from another peer.
type Transport interface {
	RoundTrip(context.Context, string, []byte, time.Duration) ([]byte, error)
}

// UDPTransport uses a connected UDP socket, which restricts received
// datagrams to the resolved peer chosen for the request.
type UDPTransport struct{}

func (UDPTransport) RoundTrip(ctx context.Context, address string, request []byte, timeout time.Duration) ([]byte, error) {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, fmt.Errorf("%w: dial bound endpoint: %v", ErrUnavailable, err)
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("rcon: set bounded deadline: %w", err)
	}
	if _, err := conn.Write(request); err != nil {
		return nil, fmt.Errorf("%w: send command: %v", ErrUnavailable, err)
	}
	response := make([]byte, maxDatagram)
	n, err := conn.Read(response)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: receive response: %v", ErrUnavailable, err)
	}
	return response[:n], nil
}

// Client is target-bound. Password bytes are retained only in memory and are
// never included in errors, evidence, or returned output.
type Client struct {
	Host      string
	Port      int
	Password  []byte
	Timeout   time.Duration
	Transport Transport
}

// ResourceAction is the closed set of RCON lifecycle operations emitted by
// reconciliation.
type ResourceAction string

const (
	ResourceRefresh ResourceAction = "refresh"
	ResourceEnsure  ResourceAction = "ensure"
	ResourceStart   ResourceAction = "start"
	ResourceStop    ResourceAction = "stop"
	ResourceRestart ResourceAction = "restart"
)

type ResourceOperation struct {
	Action ResourceAction
	Name   string
}

func (c Client) Status(ctx context.Context) (string, error) {
	return c.execute(ctx, "status")
}

func (c Client) ApplyResources(ctx context.Context, operations []ResourceOperation) error {
	for _, operation := range operations {
		command, err := resourceCommand(operation)
		if err != nil {
			return err
		}
		if _, err := c.execute(ctx, command); err != nil {
			return fmt.Errorf("rcon: %s resource %q: %w", operation.Action, operation.Name, err)
		}
	}
	return nil
}

func resourceCommand(operation ResourceOperation) (string, error) {
	switch operation.Action {
	case ResourceRefresh:
		if operation.Name != "" {
			return "", errors.New("rcon: refresh operation must not name a resource")
		}
		return "refresh", nil
	case ResourceEnsure, ResourceStart, ResourceStop, ResourceRestart:
		if err := validResourceName(operation.Name); err != nil {
			return "", err
		}
		return string(operation.Action) + " " + operation.Name, nil
	default:
		return "", fmt.Errorf("rcon: unsupported resource action %q", operation.Action)
	}
}

func validResourceName(name string) error {
	if name == "" || len(name) > 128 || name == "." || name == ".." {
		return fmt.Errorf("rcon: invalid resource name %q", name)
	}
	for _, char := range name {
		if char <= 0x20 || char == 0x7f || char == '/' || char == '\\' || char == ';' || char == '"' || char == '\'' || char == '`' {
			return fmt.Errorf("rcon: invalid resource name %q", name)
		}
	}
	return nil
}

func (c Client) execute(ctx context.Context, command string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.Host == "" || c.Port < 1 || c.Port > 65535 {
		return "", errors.New("rcon: bound host and port are required")
	}
	if len(c.Password) == 0 || len(c.Password) > 512 || bytes.ContainsAny(c.Password, " \t\r\n\x00") {
		return "", errors.New("rcon: password is missing or cannot be represented by the FiveM protocol")
	}
	if command == "" || len(command) > maxCommand || strings.ContainsAny(command, "\r\n\x00") {
		return "", errors.New("rcon: invalid command")
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	transport := c.Transport
	if transport == nil {
		transport = UDPTransport{}
	}
	request := make([]byte, 0, len(requestPrefix)+len(c.Password)+1+len(command))
	request = append(request, requestPrefix...)
	request = append(request, c.Password...)
	request = append(request, ' ')
	request = append(request, command...)
	response, err := transport.RoundTrip(ctx, net.JoinHostPort(c.Host, strconv.Itoa(c.Port)), request, timeout)
	clear(request[len(requestPrefix) : len(requestPrefix)+len(c.Password)])
	if err != nil {
		return "", err
	}
	if len(response) > maxDatagram || !bytes.HasPrefix(response, []byte(responsePrefix)) {
		return "", errors.New("rcon: malformed response from bound endpoint")
	}
	output := bytes.TrimPrefix(response, []byte(responsePrefix))
	if bytes.Equal(output, []byte("Invalid password.\n")) || bytes.Equal(output, []byte("The server must set rcon_password to be able to use this command.\n")) {
		return "", ErrAuthentication
	}
	if bytes.IndexByte(output, 0) >= 0 {
		return "", errors.New("rcon: response contains a NUL byte")
	}
	return string(bytes.TrimSuffix(output, []byte("\n"))), nil
}
