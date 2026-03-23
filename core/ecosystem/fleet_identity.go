// core/ecosystem/fleet_identity.go — Anonymous fleet machine identifier.
//
// Derives a short anonymous identifier from /etc/machine-id using SHA-256
// with a HisnOS-specific salt. The derived FleetID is safe to include in
// telemetry batches without revealing the actual machine identity.
//
// The raw machine-id is never transmitted; only the derived FleetID is used
// in external communications.
package ecosystem

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	fleetIDSalt = "hisnos-fleet-v1:"
	fleetIDLen  = 16 // hex chars from SHA-256 prefix
)

// FleetIdentity holds the anonymous identifier and its provenance.
type FleetIdentity struct {
	FleetID     string    `json:"fleet_id"`      // SHA-256[:16] of salt+machine-id — safe for telemetry
	Channel     string    `json:"channel"`       // current update channel
	HisnVersion string    `json:"hisnos_version"` // from /etc/hisnos-release
	Initialized time.Time `json:"initialized"`
}

// LoadFleetIdentity loads or derives the fleet identity.
// stateDir is typically /var/lib/hisnos.
func LoadFleetIdentity(stateDir string) (*FleetIdentity, error) {
	path := filepath.Join(stateDir, "fleet-identity.json")

	// Return cached identity if present.
	if b, err := os.ReadFile(path); err == nil {
		var id FleetIdentity
		if err := json.Unmarshal(b, &id); err == nil && id.FleetID != "" {
			return &id, nil
		}
	}

	// Derive from machine-id.
	machineID, err := readMachineID()
	if err != nil {
		return nil, fmt.Errorf("machine-id: %w", err)
	}

	h := sha256.Sum256([]byte(fleetIDSalt + machineID))
	fleetID := hex.EncodeToString(h[:])[:fleetIDLen]

	id := &FleetIdentity{
		FleetID:     fleetID,
		Channel:     "stable",
		HisnVersion: readHisnVersion(),
		Initialized: time.Now().UTC(),
	}

	// Persist for future sessions.
	if err := writeAtomic(path, mustMarshalEco(id)); err != nil {
		log.Printf("[ecosystem/fleet] WARN: persist fleet identity: %v", err)
	}
	log.Printf("[ecosystem/fleet] fleet identity initialized: fleet_id=%s", fleetID)
	return id, nil
}

// readMachineID returns the raw /etc/machine-id content.
func readMachineID() (string, error) {
	b, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// readHisnVersion reads the VERSION field from /etc/hisnos-release.
func readHisnVersion() string {
	b, err := os.ReadFile("/etc/hisnos-release")
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VERSION=") {
			return strings.TrimPrefix(line, "VERSION=")
		}
	}
	return "unknown"
}
