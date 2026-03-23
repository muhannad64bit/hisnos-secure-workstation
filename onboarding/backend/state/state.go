// state/state.go — Onboarding wizard state persistence
//
// State file: /var/lib/hisnos/onboarding-state.json
// Created on first write; must not block wizard if write fails.
//
// Fields:
//   started_at   — ISO8601 timestamp of first launch
//   completed    — true once the user finishes step 6 (verify)
//   current_step — last active step name (for resume after crash/close)
//   steps        — per-step completion flags
//
// The onboarding service disables itself on completion:
//   systemctl --user disable --now hisnos-onboarding.service

package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const DefaultStatePath = "/var/lib/hisnos/onboarding-state.json"

// StepName identifies a wizard step.
type StepName string

const (
	StepWelcome  StepName = "welcome"
	StepVault    StepName = "vault"
	StepFirewall StepName = "firewall"
	StepThreat   StepName = "threat"
	StepGaming   StepName = "gaming"
	StepVerify   StepName = "verify"
)

// AllSteps defines the canonical wizard order.
var AllSteps = []StepName{
	StepWelcome, StepVault, StepFirewall, StepThreat, StepGaming, StepVerify,
}

// StepState tracks per-step completion and configuration.
type StepState struct {
	Completed bool   `json:"completed"`
	SkippedAt string `json:"skipped_at,omitempty"`
	Notes     string `json:"notes,omitempty"`
}

// OnboardingState is the full persisted state.
type OnboardingState struct {
	StartedAt   string               `json:"started_at"`
	CompletedAt string               `json:"completed_at,omitempty"`
	Completed   bool                 `json:"completed"`
	CurrentStep StepName             `json:"current_step"`
	Steps       map[StepName]*StepState `json:"steps"`

	// Configuration choices persisted for later introspection.
	FirewallProfile    string   `json:"firewall_profile,omitempty"`   // strict|balanced|gaming-ready
	ThreatNotify       bool     `json:"threat_notifications"`
	GamingGroupEnabled bool     `json:"gaming_group_enabled"`
	VaultInitialized   bool     `json:"vault_initialized"`
	Warnings           []string `json:"warnings,omitempty"`
}

// Manager owns state persistence with mutex protection.
type Manager struct {
	mu   sync.Mutex
	path string
	s    OnboardingState
}

// NewManager creates a state Manager and loads existing state.
func NewManager(path string) *Manager {
	m := &Manager{path: path}
	m.load()
	return m
}

// Get returns a copy of the current state.
func (m *Manager) Get() OnboardingState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s
}

// Update applies fn to state and persists atomically.
func (m *Manager) Update(fn func(*OnboardingState)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.s)
	return m.persist()
}

// MarkStepComplete marks a step as completed.
func (m *Manager) MarkStepComplete(step StepName) error {
	return m.Update(func(s *OnboardingState) {
		if s.Steps == nil {
			s.Steps = make(map[StepName]*StepState)
		}
		s.Steps[step] = &StepState{Completed: true}
		s.CurrentStep = nextStep(step)
	})
}

// MarkComplete finalises the wizard.
func (m *Manager) MarkComplete() error {
	return m.Update(func(s *OnboardingState) {
		s.Completed = true
		s.CompletedAt = time.Now().UTC().Format(time.RFC3339)
		s.CurrentStep = StepVerify
	})
}

// AddWarning appends a non-fatal warning message.
func (m *Manager) AddWarning(msg string) error {
	return m.Update(func(s *OnboardingState) {
		s.Warnings = append(s.Warnings, fmt.Sprintf("[%s] %s",
			time.Now().UTC().Format(time.RFC3339), msg))
	})
}

// IsCompleted returns true if onboarding was already finished.
func (m *Manager) IsCompleted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.s.Completed
}

// load reads state from disk. Non-fatal if file missing.
func (m *Manager) load() {
	data, err := os.ReadFile(m.path)
	if err != nil {
		// First run — initialise.
		m.s = OnboardingState{
			StartedAt:   time.Now().UTC().Format(time.RFC3339),
			CurrentStep: StepWelcome,
			Steps:       make(map[StepName]*StepState),
		}
		return
	}
	if err := json.Unmarshal(data, &m.s); err != nil {
		m.s = OnboardingState{
			StartedAt:   time.Now().UTC().Format(time.RFC3339),
			CurrentStep: StepWelcome,
			Steps:       make(map[StepName]*StepState),
			Warnings:    []string{"state file corrupt — reset to initial state"},
		}
	}
	if m.s.Steps == nil {
		m.s.Steps = make(map[StepName]*StepState)
	}
}

// persist writes state atomically.
func (m *Manager) persist() error {
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".onboarding-state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { tmp.Close(); os.Remove(name) }()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	tmp.Close()
	return os.Rename(name, m.path)
}

// nextStep returns the step that follows step in AllSteps.
func nextStep(step StepName) StepName {
	for i, s := range AllSteps {
		if s == step && i+1 < len(AllSteps) {
			return AllSteps[i+1]
		}
	}
	return StepVerify
}
