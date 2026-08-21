// Command sdk-consumer is a fixture third-party Go program that drives FXVCS
// exclusively through the public SDK (fxvcs.dev/fxvcs/sdk). It imports no
// internal package and parses no CLI text. Its JSON output must equal the
// CLI's --json output for the same operation, which the parity test enforces.
//
// Usage: sdk-consumer <operation> [path]
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"fxvcs.dev/fxvcs/sdk"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sdk-consumer:", err)
		os.Exit(1)
	}
}

// Run executes one operation and writes its JSON result plus a JSON event log
// (one line per progress event) to stderr.
func run(args []string, stdout, stderr *os.File) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: sdk-consumer <operation> [path]")
	}
	op := args[0]
	path := "."
	if len(args) > 1 {
		path = args[1]
	}
	events := json.NewEncoder(stderr)
	client, err := sdk.New(sdk.Options{Progress: func(ev sdk.Event) { _ = events.Encode(ev) }})
	if err != nil {
		return err
	}
	res, err := Dispatch(context.Background(), client, op, path)
	if err != nil {
		var se *sdk.Error
		if ok := asSDKError(err, &se); ok {
			enc := json.NewEncoder(stderr)
			return enc.Encode(map[string]any{"error": map[string]any{"code": se.Code, "message": se.Error(), "details": se.Details}})
		}
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func asSDKError(err error, target **sdk.Error) bool {
	for err != nil {
		if e, ok := err.(*sdk.Error); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Dispatch maps an operation name to the SDK call. Only typed requests and
// results cross this boundary.
func Dispatch(ctx context.Context, c *sdk.Client, op, path string) (any, error) {
	switch op {
	case "compat":
		return c.Compatibility(ctx, sdk.CompatibilityRequest{Path: path})
	case "storage-list":
		return c.StorageList(ctx, sdk.StorageListRequest{Path: path})
	case "objects-status":
		return c.ObjectsStatus(ctx, sdk.ObjectsStatusRequest{Path: path})
	case "cache-status":
		return c.CacheStatus(ctx, sdk.CacheStatusRequest{Path: path})
	case "status":
		return c.Status(ctx, sdk.StatusRequest{Path: path})
	case "verify":
		return c.Verify(ctx, sdk.VerifyRequest{Path: path})
	case "doctor":
		return c.Doctor(ctx, sdk.DoctorRequest{Path: path})
	case "resources-validate":
		return c.ResourcesValidate(ctx, sdk.ResourcesValidateRequest{Path: path})
	case "deployments-list":
		return c.DeploymentsList(ctx, sdk.DeploymentsListRequest{AgentConfigPath: path})
	}
	return nil, fmt.Errorf("unknown operation %q", op)
}
