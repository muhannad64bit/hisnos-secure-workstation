// core/ecosystem/telemetry_client.go — Optional anonymous usage telemetry.
//
// Disabled by default. Enable by setting enabled=true in /etc/hisnos/telemetry.conf.
//
// Privacy guarantees:
//   - FleetID is a derived hash — machine-id is never transmitted.
//   - No usernames, hostnames, IP addresses, or file contents are collected.
//   - Collected: hisnos version, channel, feature flags, anonymized error counts.
//   - Batches are written locally to /var/lib/hisnos/telemetry-batch.jsonl.
//   - HTTP POST occurs only when endpoint is configured and enabled=true.
//
// Batch rotation: new batch every 24h; max 7 batches retained.
package ecosystem

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	telemetryConfig = "/etc/hisnos/telemetry.conf"
	batchMaxAge     = 24 * time.Hour
	maxBatches      = 7
	httpTimeout     = 10 * time.Second
)

// TelemetryEvent is a single anonymized usage event.
type TelemetryEvent struct {
	FleetID   string    `json:"fleet_id"`
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`
	Channel   string    `json:"channel"`
	Version   string    `json:"version"`
	Data      map[string]any `json:"data,omitempty"`
}

// TelemetryClient batches and optionally ships anonymous usage events.
type TelemetryClient struct {
	enabled   bool
	endpoint  string
	fleetID   string
	version   string
	channel   string
	batchDir  string
	httpClient *http.Client
}

// NewTelemetryClient loads configuration and initialises the client.
func NewTelemetryClient(stateDir string, identity *FleetIdentity) *TelemetryClient {
	tc := &TelemetryClient{
		fleetID:  identity.FleetID,
		version:  identity.HisnVersion,
		channel:  identity.Channel,
		batchDir: stateDir,
		httpClient: &http.Client{Timeout: httpTimeout},
	}
	tc.loadConfig()
	if tc.enabled {
		log.Printf("[ecosystem/telemetry] enabled → endpoint=%s", tc.endpoint)
	} else {
		log.Printf("[ecosystem/telemetry] disabled (set enabled=true in %s to enable)", telemetryConfig)
	}
	return tc
}

// Enabled returns whether telemetry collection is active.
func (tc *TelemetryClient) Enabled() bool { return tc.enabled }

// Record queues an anonymous event. No-op when disabled.
func (tc *TelemetryClient) Record(eventType string, data map[string]any) {
	if !tc.enabled {
		return
	}
	ev := TelemetryEvent{
		FleetID:   tc.fleetID,
		Timestamp: time.Now().UTC(),
		Type:      eventType,
		Channel:   tc.channel,
		Version:   tc.version,
		Data:      data,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	batchPath := tc.currentBatchPath()
	f, err := os.OpenFile(batchPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		log.Printf("[ecosystem/telemetry] WARN: open batch: %v", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\n", b)
}

// Flush sends the current batch to the configured endpoint and rotates.
// No-op when disabled or no endpoint is configured.
func (tc *TelemetryClient) Flush() error {
	if !tc.enabled || tc.endpoint == "" {
		return nil
	}
	batchPath := tc.currentBatchPath()
	if _, err := os.Stat(batchPath); os.IsNotExist(err) {
		return nil // nothing to flush
	}

	events, err := tc.readBatch(batchPath)
	if err != nil || len(events) == 0 {
		return err
	}

	payload, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		return err
	}

	resp, err := tc.httpClient.Post(tc.endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telemetry POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("telemetry POST: server returned %d", resp.StatusCode)
	}

	// Rotate batch on success.
	_ = os.Rename(batchPath, batchPath+".sent."+time.Now().Format("20060102"))
	tc.pruneOldBatches()
	log.Printf("[ecosystem/telemetry] flushed %d events", len(events))
	return nil
}

// currentBatchPath returns the path for today's batch file.
func (tc *TelemetryClient) currentBatchPath() string {
	return filepath.Join(tc.batchDir, "telemetry-batch.jsonl")
}

// readBatch parses a JSONL batch file into a slice of events.
func (tc *TelemetryClient) readBatch(path string) ([]TelemetryEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []TelemetryEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev TelemetryEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err == nil {
			events = append(events, ev)
		}
	}
	return events, sc.Err()
}

// pruneOldBatches removes sent batch files beyond maxBatches.
func (tc *TelemetryClient) pruneOldBatches() {
	entries, err := os.ReadDir(tc.batchDir)
	if err != nil {
		return
	}
	var sentFiles []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "telemetry-batch") && strings.Contains(e.Name(), ".sent.") {
			sentFiles = append(sentFiles, filepath.Join(tc.batchDir, e.Name()))
		}
	}
	if len(sentFiles) > maxBatches {
		for _, f := range sentFiles[:len(sentFiles)-maxBatches] {
			_ = os.Remove(f)
		}
	}
}

// loadConfig parses /etc/hisnos/telemetry.conf (key=value format).
func (tc *TelemetryClient) loadConfig() {
	b, err := os.ReadFile(telemetryConfig)
	if err != nil {
		return // file absent = disabled (default)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "enabled":
			tc.enabled = strings.TrimSpace(v) == "true"
		case "endpoint":
			tc.endpoint = strings.TrimSpace(v)
		}
	}
}
