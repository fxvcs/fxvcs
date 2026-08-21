package cli

import (
	"context"
	"fmt"

	"fxvcs.dev/fxvcs/internal/cli/ui"
	"fxvcs.dev/fxvcs/sdk"
	"github.com/urfave/cli/v3"
)

func deploymentCommands(env Env) []*cli.Command {
	return []*cli.Command{
		group("deployments", catStack, "Inspect and safely prune target-local deployment history",
			deploymentsListCommand(env),
			deploymentsPruneCommand(env),
		),
	}
}

func agentConfigFlag() cli.Flag {
	return &cli.StringFlag{Name: "agent-config", Usage: "host-local AgentConfig path (default: FXVCS_AGENT_CONFIG or /etc/fxvcs/agent.yaml)"}
}

func deploymentsListCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "list",
		Usage:     "Show current, last-known-good, and durable deployment attempts for bound targets",
		ArgsUsage: "[target]",
		Examples: []string{
			"fxvcs deployments list",
			"fxvcs deployments list dev",
			"fxvcs deployments list prd --agent-config /etc/fxvcs/agent.yaml",
		},
		Flags: []cli.Flag{agentConfigFlag()},
		Op: func(ctx context.Context, c *sdk.Client, _ string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() > 1 {
				return nil, usageError{"deployments list accepts at most one target", "fxvcs deployments list [target]"}
			}
			return c.DeploymentsList(ctx, sdk.DeploymentsListRequest{AgentConfigPath: cmd.String("agent-config"), Target: cmd.Args().First()})
		},
		Render: renderDeploymentsList,
	})
}

func deploymentsPruneCommand(env Env) *cli.Command {
	return command(env, cmdSpec{
		Name:      "prune",
		Usage:     "Plan removal of old target realizations (dry run unless --apply)",
		ArgsUsage: "<target>",
		Examples: []string{
			"fxvcs deployments prune dev",
			"fxvcs deployments prune dev --keep 5",
			"fxvcs deployments prune dev --apply --plan prune_0123456789abcdef01234567",
		},
		Flags: []cli.Flag{
			agentConfigFlag(),
			&cli.IntFlag{Name: "keep", Usage: "minimum number of newest realizations to keep (cannot weaken the host floor)"},
			&cli.BoolFlag{Name: "apply", Usage: "apply the exact plan after revalidating every protection under the target lock"},
			&cli.StringFlag{Name: "plan", Usage: "expected plan ID from a reviewed dry run; stale plans are refused"},
		},
		Op: func(ctx context.Context, c *sdk.Client, _ string, cmd *cli.Command) (any, error) {
			if cmd.Args().Len() != 1 {
				return nil, usageError{"deployments prune needs exactly one target", "fxvcs deployments prune <target> [--keep N] [--apply]"}
			}
			return c.DeploymentsPrune(ctx, sdk.DeploymentsPruneRequest{
				AgentConfigPath: cmd.String("agent-config"),
				Target:          cmd.Args().First(),
				Keep:            cmd.Int("keep"),
				Apply:           cmd.Bool("apply"),
				ExpectedPlanID:  cmd.String("plan"),
			})
		},
		Render: renderDeploymentsPrune,
	})
}

func renderDeploymentsList(p *ui.Printer, value any, _ renderMeta) {
	result := value.(*sdk.DeploymentsListResult)
	p.Header("deployments list", ui.Count(len(result.Targets), "bound target"))
	for _, target := range result.Targets {
		current := target.CurrentRealizationID
		if current == "" {
			current = "none"
		}
		lkg := target.LastKnownGoodRealizationID
		if lkg == "" {
			lkg = "none"
		}
		p.Blank()
		p.Line("  %s: current %s; last known good %s; %s", target.Target, ui.ShortID(current), ui.ShortID(lkg), ui.Count(len(target.Deployments), "attempt"))
		rows := make([][]string, 0, len(target.Deployments))
		for _, deployment := range target.Deployments {
			note := deployment.Outcome
			if deployment.RetryHeld {
				note = "retry held; " + note
			}
			if deployment.AttemptInvalidated {
				note = "attempt invalidated; " + note
			}
			rows = append(rows, []string{ui.ShortID(deployment.RealizationID), deployment.Release,
				fmt.Sprintf("%s@%d", deployment.Environment, deployment.EnvironmentRevision), deployment.Phase, deployment.StartedAt, note})
		}
		if len(rows) > 0 {
			p.Table(rows)
		}
		p.Verbosef("deployment root %s", target.DeployRoot)
	}
}

func renderDeploymentsPrune(p *ui.Printer, value any, _ renderMeta) {
	result := value.(*sdk.DeploymentsPruneResult)
	verb := "would remove"
	if result.Applied {
		verb = "removed"
	}
	p.Header("deployments prune", fmt.Sprintf("%s %s (%s); keeping at least %d",
		verb, ui.Count(len(result.Candidates), "realization"), ui.Bytes(result.EstimatedBytes), result.Keep))
	if len(result.MaterializedCandidates) > 0 {
		p.Line("  %s %s of unreferenced materialized content", verb, ui.Bytes(result.EstimatedMaterializedBytes))
	}
	if len(result.Candidates) > 0 {
		p.Blank()
		rows := make([][]string, 0, len(result.Candidates))
		for _, candidate := range result.Candidates {
			rows = append(rows, []string{ui.ShortID(candidate.RealizationID), ui.Bytes(candidate.Bytes)})
			p.Verbosef("candidate %s", candidate.Path)
		}
		p.Table(rows)
	}
	for _, candidate := range result.MaterializedCandidates {
		p.Verbosef("materialized candidate %s (%s, %s)", candidate.Digest, candidate.Path, ui.Bytes(candidate.Bytes))
	}
	p.Verbosef("plan %s", result.PlanID)
	if !result.Applied && (len(result.Candidates) > 0 || len(result.MaterializedCandidates) > 0) {
		p.Footer(fmt.Sprintf("nothing was removed. Apply this reviewed plan with: fxvcs deployments prune %s --apply --plan %s", result.Target, result.PlanID))
	}
}
