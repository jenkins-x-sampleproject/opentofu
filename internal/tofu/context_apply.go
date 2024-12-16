// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package tofu

import (
	"fmt"
	"log"

	"github.com/zclconf/go-cty/cty"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

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
		// Import is a no-op change during an apply (all the real action happens during the plan) but we'd
		// like to show some helpful output that mirrors the way we show other changes.
		if rc.Importing != nil {
			for _, h := range c.hooks {
				if hookDiags := handleImportHooks(h, rc.Addr, rc.Importing); hookDiags.HasErrors() {
					diags = diags.Append(hookDiags)
				}
			}
		}

		// Following the same logic, we want to show helpful output for forget operations as well.
		if rc.Action == plans.Forget {
			for _, h := range c.hooks {
				if hookDiags := handleForgetHooks(h, rc.Addr); hookDiags.HasErrors() {
					diags = diags.Append(hookDiags)
				}
			}
		}
	}

	graph, operation, diags := c.applyGraph(plan, config, true)
	if diags.HasErrors() {
		return nil, diags
	}

	workingState := plan.PriorState.DeepCopy()
	walker, walkDiags := c.walk(c, graph, operation, &graphWalkOpts{
		Config:     config,
		InputState: workingState,
		Changes:    plan.Changes,
		PlanTimeCheckResults: plan.Checks,
		PlanTimeTimestamp:   plan.Timestamp,
	})
	diags = diags.Append(walker.NonFatalDiagnostics)
	diags = diags.Append(walkDiags)

	// After the walk is finished, we capture a simplified snapshot of the
	// check result data as part of the new state.
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

func (c *Context) applyGraph(plan *plans.Plan, config *configs.Config, validate bool) (*Graph, walkOperation, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	variables := InputValues{}
	for name, dyVal := range plan.VariableValues {
		val, err := dyVal.Decode(cty.DynamicPseudoType)
		if err != nil {
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid variable value in plan",
				fmt.Sprintf("Invalid value for variable %q recorded in plan file: %s.", name, err),
			))
			continue
		}

		variables[name] = &InputValue{
			Value:      val,
			SourceType: ValueFromPlan,
		}
	}
	if diags.HasErrors() {
		return nil, walkApply, diags
	}

	for name := range config.Module.Variables {
		if _, ok := variables[name]; ok {
			continue
		}
		variables[name] = &InputValue{
			Value:      cty.NilVal,
			SourceType: ValueFromPlan,
		}
	}

	operation := walkApply
	if plan.UIMode == plans.DestroyMode {
		operation = walkDestroy
	}

	graph, moreDiags := (&ApplyGraphBuilder{
		Config:             config,
		Changes:            plan.Changes,
		State:              plan.PriorState,
		RootVariableValues: variables,
		Plugins:            c.plugins,
		Targets:            plan.TargetAddrs,
		ForceReplace:       plan.ForceReplaceAddrs,
		Operation:          operation,
		ExternalReferences: plan.ExternalReferences,
	}).Build(addrs.RootModuleInstance)
	diags = diags.Append(moreDiags)
	if moreDiags.HasErrors() {
		return nil, walkApply, diags
	}

	return graph, operation, diags
}

func (c *Context) ApplyGraphForUI(plan *plans.Plan, config *configs.Config) (*Graph, tfdiags.Diagnostics) {
	// For now though, this really is just the internal graph, confusing
	// implementation details and all.

	var diags tfdiags.Diagnostics

	graph, _, moreDiags := c.applyGraph(plan, config, false)
	diags = diags.Append(moreDiags)
	return graph, diags
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

// handleForgetHooks manages the hooks for the Forget operation.
func handleForgetHooks(h Hook, addr addrs.AbsResourceInstance) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if _, err := h.PreApplyForget(addr); err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"PreApplyForget hook failed",
			err.Error(),
		))
	}

	if _, err := h.PostApplyForget(addr); err != nil {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Error,
			"PostApplyForget hook failed",
			err.Error(),
		))
	}

	return diags
}
