// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"context"
	"fmt"
	"log"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

// Apply applies the given plan and configuration, returning the resulting state and diagnostics.
func (c *Context) Apply(plan *plans.Plan, config *configs.Config) (*states.State, tfdiags.Diagnostics) {
	defer c.acquireRun("apply")()

	log.Printf("[DEBUG] Building and walking apply graph for %s plan", plan.UIMode)

	if plan.Errored {
		var diags tfdiags.Diagnostics
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"Cannot apply failed plan",
			`The given plan is incomplete due to errors during planning, and so it cannot be applied.`,
		))
		return nil, diags
	}

	var diags tfdiags.Diagnostics
	for _, rc := range plan.Changes.Resources {
		// Handle import hooks
		if rc.Importing != nil {
			for _, h := range c.hooks {
				if hookDiags := handleImportHooks(h, rc.Addr, rc.Importing); hookDiags.HasErrors() {
					diags = diags.Append(hookDiags)
				}
			}
		}

		// Skipping forget hooks as `PreApplyForget` and `PostApplyForget` are undefined
		if rc.Action == plans.Forget {
			log.Printf("[DEBUG] Forget action detected for resource: %s", rc.Addr)
		}
	}

	// Apply graph changes
	ctx := context.Background()
	graph, operation, graphDiags := c.applyGraph(plan, config, true)
	diags = diags.Append(graphDiags)
	if diags.HasErrors() {
		return nil, diags
	}

	workingState := plan.PriorState.DeepCopy()
	walker, walkDiags := c.walk(ctx, graph, operation, &graphWalkOpts{
		Config:                config,
		InputState:            workingState,
		Changes:               plan.Changes,
		PlanTimeCheckResults:  plan.Checks,
		PlanTimeTimestamp:     plan.Timestamp,
	})
	diags = diags.Append(walker.NonFatalDiagnostics)
	diags = diags.Append(walkDiags)

	walker.State.RecordCheckResults(walker.Checks)

	newState := walker.State.Close()
	if plan.UIMode == plans.DestroyMode && !diags.HasErrors() {
		newState.PruneResourceHusks()
	}

	if len(plan.TargetAddrs) > 0 {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Warning,
			"Applied changes may be incomplete",
			`The plan was created with the -target option in effect, so some changes requested in the configuration may have been ignored and the output values may not be fully updated. Run the following command to verify that no other changes are pending:
    tofu plan
	
Note that the -target option is not suitable for routine use, and is provided only for exceptional situations such as recovering from errors or mistakes, or when OpenTofu specifically suggests to use it as part of an error message.`,
		))
	}

	if plan.UIMode == plans.RefreshOnlyMode {
		newState.CheckResults = plan.Checks.DeepCopy()
	}

	return newState, diags
}

// handleImportHooks manages the hooks for the Importing operation.
func handleImportHooks(h Hook, addr addrs.AbsResourceInstance, importing *plans.ImportingSrc) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if _, err := h.PreApplyImport(addr, importing); err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"PreApplyImport hook failed",
			err.Error(),
		))
	}

	if _, err := h.PostApplyImport(addr, importing); err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"PostApplyImport hook failed",
			err.Error(),
		))
	}

	return diags
}
