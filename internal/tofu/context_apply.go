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
type ForgetAction struct {
	Details string // Example field for ForgetAction
}

// Apply performs the actions described by the given Plan object and returns
// the resulting updated state.
func (c *Context) Apply(ctx context.Context, plan *plans.Plan, config *configs.Config) (*states.State, tfdiags.Diagnostics) {
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

	// Iterate through the resources and process the different actions
	for _, rc := range plan.Changes.Resources {
		// If the resource is being imported, process the import action
		if rc.Importing != nil {
			for _, h := range c.hooks {
				h.PreApplyImport(rc.Addr, *rc.Importing)
				h.PostApplyImport(rc.Addr, *rc.Importing)
			}
		} else if rc.Forgetting != nil { // If the resource is being forgotten
			for _, h := range c.hooks {
				h.PreApplyForget(rc.Addr, *rc.Forgetting)
				h.PostApplyForget(rc.Addr, *rc.Forgetting)
			}
			// Remove the resource from the state
			log.Printf("[INFO] Forgetting resource: %s", rc.Addr)
			// Assuming removeResourceFromState is a method to remove the resource from state
			c.removeResourceFromState(rc.Addr)
		}
	}

	providerFunctionTracker := make(ProviderFunctionMapping)

	// Apply the graph to update the state
	graph, operation, diags := c.applyGraph(plan, config, true, providerFunctionTracker)
	if diags.HasErrors() {
		return nil, diags
	}

	// Deep copy the previous state and apply the changes
	workingState := plan.PriorState.DeepCopy()
	walker, walkDiags := c.walk(ctx, graph, operation, &graphWalkOpts{
		Config:     config,
		InputState: workingState,
		Changes:    plan.Changes,
		PlanTimeCheckResults: plan.Checks,
		PlanTimeTimestamp:    plan.Timestamp,
		ProviderFunctionTracker: providerFunctionTracker,
	})
	diags = diags.Append(walker.NonFatalDiagnostics)
	diags = diags.Append(walkDiags)

	// Finalize state after applying changes
	walker.State.RecordCheckResults(walker.Checks)

	newState := walker.State.Close()
	if plan.UIMode == plans.DestroyMode && !diags.HasErrors() {
		newState.PruneResourceHusks()
	}

	// Handle warnings related to target/exclude filters
	if len(plan.TargetAddrs) > 0 || len(plan.ExcludeAddrs) > 0 {
		diags = diags.Append(tfdiags.Sourceless(
			tfdiags.Warning,
			"Applied changes may be incomplete",
			`The plan was created with the -target or the -exclude option in effect, so some changes requested in the configuration may have been ignored and the output values may not be fully updated. Run the following command to verify that no other changes are pending:
    tofu plan
	
Note that the -target and -exclude options are not suitable for routine use, and are provided only for exceptional situations such as recovering from errors or mistakes, or when OpenTofu specifically suggests to use it as part of an error message.`,
		))
	}

	// Handle refresh-only plans
	if plan.UIMode == plans.RefreshOnlyMode {
		newState.CheckResults = plan.Checks.DeepCopy()
	}

	return newState, diags
}
func (c *Context) removeResourceFromState(addr addrs.ResourceAddr) {
	log.Printf("[DEBUG] Removing resource %s from state", addr)
	// Implement the logic to remove the resource from the state
}

type Hook interface {
	PreApplyImport(addr addrs.ResourceAddr, action ForgetAction)
	PostApplyImport(addr addrs.ResourceAddr, action ForgetAction)
	PreApplyForget(addr addrs.ResourceAddr, action ForgetAction)
	PostApplyForget(addr addrs.ResourceAddr, action ForgetAction)
}

type ResourceInstanceChangeSrc struct {
	Importing  *ForgetAction
	Forgetting *ForgetAction
}

//nolint:revive,unparam // TODO remove validate bool as it's not used
func (c *Context) applyGraph(plan *plans.Plan, config *configs.Config, validate bool, providerFunctionTracker ProviderFunctionMapping) (*Graph, walkOperation, tfdiags.Diagnostics) {
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

	// The plan.VariableValues field only records variables that were actually
	// set by the caller in the PlanOpts, so we may need to provide
	// placeholders for any other variables that the user didn't set, in
	// which case OpenTofu will once again use the default value from the
	// configuration when we visit these variables during the graph walk.
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
		Config:                  config,
		Changes:                 plan.Changes,
		State:                   plan.PriorState,
		RootVariableValues:      variables,
		Plugins:                 c.plugins,
		Targets:                 plan.TargetAddrs,
		Excludes:                plan.ExcludeAddrs,
		ForceReplace:            plan.ForceReplaceAddrs,
		Operation:               operation,
		ExternalReferences:      plan.ExternalReferences,
		ProviderFunctionTracker: providerFunctionTracker,
	}).Build(addrs.RootModuleInstance)
	diags = diags.Append(moreDiags)
	if moreDiags.HasErrors() {
		return nil, walkApply, diags
	}

	return graph, operation, diags
}

// ApplyGraphForUI is a last vestige of graphs in the public interface of
// Context (as opposed to graphs as an implementation detail) intended only for
// use by the "tofu graph" command when asked to render an apply-time
// graph.
func (c *Context) ApplyGraphForUI(plan *plans.Plan, config *configs.Config) (*Graph, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics

	graph, _, moreDiags := c.applyGraph(plan, config, false, make(ProviderFunctionMapping))
	diags = diags.Append(moreDiags)
	return graph, diags
}
