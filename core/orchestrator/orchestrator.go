// core/orchestrator/orchestrator.go — Orchestrator interface and dispatcher.
//
// Each orchestrator owns one subsystem (vault, firewall, lab, gaming, update,
// threat). It receives Action objects from the policy engine, executes the
// corresponding system operation (shell script, systemctl, nft), and reports
// success or failure via error return.
//
// Orchestrators must NOT publish events themselves — the Dispatcher does that
// after receiving the error result. This keeps each orchestrator testable as
// a pure system-call wrapper.

package orchestrator

import (
	"fmt"
	"hisnos.local/hisnosd/policy"
)

// Orchestrator executes a single Action and verifies subsystem health.
type Orchestrator interface {
	// Execute performs the system operation for the given action.
	Execute(action policy.Action) error
	// HealthCheck verifies the subsystem is reachable and alive.
	// Returns nil if healthy, non-nil with a description if not.
	HealthCheck() error
}

// Registry maps ActionTypes to the orchestrator responsible for them.
type Registry struct {
	m map[policy.ActionType]Orchestrator
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[policy.ActionType]Orchestrator)}
}

// Register associates an ActionType with an Orchestrator.
func (r *Registry) Register(t policy.ActionType, o Orchestrator) {
	r.m[t] = o
}

// Dispatch finds the orchestrator for action.Type and calls Execute.
// Returns an error if no orchestrator is registered or Execute fails.
func (r *Registry) Dispatch(action policy.Action) error {
	o, ok := r.m[action.Type]
	if !ok {
		return fmt.Errorf("no orchestrator registered for action %s", action.Type)
	}
	return o.Execute(action)
}

// HealthAll runs HealthCheck on every registered orchestrator.
// Returns a map of ActionType → error (nil = healthy).
func (r *Registry) HealthAll() map[policy.ActionType]error {
	results := make(map[policy.ActionType]error, len(r.m))
	for t, o := range r.m {
		results[t] = o.HealthCheck()
	}
	return results
}
