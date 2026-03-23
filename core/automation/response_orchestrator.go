// core/automation/response_orchestrator.go — Pre-emptive security action dispatcher.
//
// Maps cluster patterns and risk predictions to pre-emptive policy actions.
// All actions are subject to per-action cooldowns to prevent thrashing.
// Actions are submitted through the IPC command pathway (same as operator commands)
// so they flow through the PolicyEnforcer and safe-mode gate.
//
// Pre-emptive rules are evaluated in priority order; only the highest-priority
// matching rule fires per evaluation cycle.
package automation

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// ActionFn is the function the orchestrator uses to submit actions.
// It receives the IPC command name and params, returning an error on failure.
// Wired at construction time (e.g. to ipc.Server.SubmitCommand or a test stub).
type ActionFn func(command string, params map[string]any) error

// preemptiveRule defines when to trigger an action set.
type preemptiveRule struct {
	pattern  string        // cluster pattern to match
	minScore float64       // minimum cluster strength to trigger
	actions  []string      // IPC commands to submit
	cooldown time.Duration // minimum time between consecutive triggers
	priority int           // lower = higher priority (evaluated first)
}

// rules are evaluated in ascending priority order.
var rules = []preemptiveRule{
	{pattern: "lateral_movement", minScore: 55.0, actions: []string{"reload_firewall", "stop_lab"}, cooldown: 3 * time.Minute, priority: 1},
	{pattern: "kernel_exploit", minScore: 60.0, actions: []string{"reload_firewall", "lock_vault"}, cooldown: 2 * time.Minute, priority: 2},
	{pattern: "exfil_prep", minScore: 50.0, actions: []string{"lock_vault", "reload_firewall"}, cooldown: 2 * time.Minute, priority: 3},
	{pattern: "persistence_rootkit", minScore: 65.0, actions: []string{"reload_firewall"}, cooldown: 5 * time.Minute, priority: 4},
	{pattern: "escalation", minScore: 65.0, actions: []string{"reload_firewall"}, cooldown: 5 * time.Minute, priority: 5},
	{pattern: "generic", minScore: 75.0, actions: []string{"reload_firewall"}, cooldown: 5 * time.Minute, priority: 6},
}

// ResponseOrchestrator dispatches pre-emptive actions based on cluster patterns.
type ResponseOrchestrator struct {
	submit    ActionFn
	cooldowns map[string]time.Time // pattern → last trigger time
	mu        sync.Mutex
}

// NewResponseOrchestrator creates an orchestrator that submits actions via submit.
func NewResponseOrchestrator(submit ActionFn) *ResponseOrchestrator {
	return &ResponseOrchestrator{
		submit:    submit,
		cooldowns: make(map[string]time.Time),
	}
}

// DispatchResult records which actions were fired in a dispatch cycle.
type DispatchResult struct {
	Pattern   string
	Actions   []string
	Skipped   bool   // true if cooldown prevented dispatch
	SkipReason string
}

// Dispatch evaluates clusters against pre-emptive rules and fires matching actions.
// Returns the dispatch result for observability. Only the highest-priority rule fires.
func (o *ResponseOrchestrator) Dispatch(clusters []Cluster) *DispatchResult {
	if len(clusters) == 0 {
		return nil
	}

	// Find highest-priority matching rule across all hot clusters.
	var bestRule *preemptiveRule
	var bestCluster *Cluster
	for ri := range rules {
		r := &rules[ri]
		for ci := range clusters {
			c := &clusters[ci]
			if !c.Hot {
				continue
			}
			if c.Pattern != r.pattern && r.pattern != "generic" {
				continue
			}
			if c.Strength < r.minScore {
				continue
			}
			if bestRule == nil || r.priority < bestRule.priority {
				bestRule = r
				bestCluster = c
			}
		}
		if bestRule != nil {
			break
		}
	}

	if bestRule == nil {
		return nil
	}

	// Check cooldown.
	o.mu.Lock()
	lastFire, fired := o.cooldowns[bestRule.pattern]
	if fired && time.Since(lastFire) < bestRule.cooldown {
		o.mu.Unlock()
		remaining := time.Until(lastFire.Add(bestRule.cooldown)).Round(time.Second)
		return &DispatchResult{
			Pattern:    bestRule.pattern,
			Skipped:    true,
			SkipReason: fmt.Sprintf("cooldown: %v remaining", remaining),
		}
	}
	o.cooldowns[bestRule.pattern] = time.Now()
	o.mu.Unlock()

	// Fire all actions.
	var fired_actions []string
	for _, cmd := range bestRule.actions {
		params := map[string]any{"source": "automation", "cluster_id": bestCluster.ID}
		if err := o.submit(cmd, params); err != nil {
			log.Printf("[automation/resp] action %s failed: %v", cmd, err)
			continue
		}
		fired_actions = append(fired_actions, cmd)
		log.Printf("[automation/resp] pre-emptive: pattern=%s action=%s cluster=%s strength=%.1f",
			bestRule.pattern, cmd, bestCluster.ID, bestCluster.Strength)
	}

	return &DispatchResult{
		Pattern: bestRule.pattern,
		Actions: fired_actions,
	}
}

// GamingThrottle requests gaming mode stop (called when risk exceeds threshold in gaming mode).
func (o *ResponseOrchestrator) GamingThrottle() error {
	return o.submit("stop_gaming", map[string]any{"source": "automation", "reason": "risk_threshold"})
}
