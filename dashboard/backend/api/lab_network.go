// api/lab_network.go — Lab network containment profile API
//
// Routes:
//   GET  /api/lab/network-profile  — read current stored profile from control plane state
//   POST /api/lab/network-profile  — set profile; rejected if a session is active
//
// The network profile is stored in cpstate.LabFacts.NetworkProfile.
// It is applied when the next session starts (POST /api/lab/start reads it).
// Profile changes are blocked while a session is active to prevent
// mid-session policy changes that could bypass containment.
//
// Profiles:
//   offline         No network (bwrap --unshare-net only, no veth). DEFAULT.
//   allowlist-cidr  Veth pair + nftables; only CIDRs in allowed_cidrs are reachable.
//   dns-sinkhole    Veth pair; DNS intercepted by hisnos-lab-dns-sinkhole.py → NXDOMAIN.
//   http-proxy      Veth pair; outbound only to specified proxy_addr host:port.
//
// Journal event:
//   HISNOS_LAB_NET_PROFILE profile=<new> session=none (logged by this handler)
//
// Safety guarantee (enforced by LabStart handler, not here):
//   If netd setup fails at session start, the session falls back to offline.
//   The stored profile is NOT modified on fallback — it remains the operator intent.

package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	execpkg "hisnos.local/dashboard/exec"
	cpstate "hisnos.local/dashboard/state"
)

// LabNetworkProfileResponse is the JSON payload for GET /api/lab/network-profile.
type LabNetworkProfileResponse struct {
	NetworkProfile string   `json:"network_profile"`
	AllowedCIDRs   []string `json:"allowed_cidrs,omitempty"`
	ProxyAddr      string   `json:"proxy_addr,omitempty"`
	SessionActive  bool     `json:"session_active"`
	// Transitions blocked while session_active = true.
}

// LabNetworkProfileRequest is the JSON body for POST /api/lab/network-profile.
type LabNetworkProfileRequest struct {
	NetworkProfile string   `json:"network_profile"`         // required
	AllowedCIDRs   []string `json:"allowed_cidrs,omitempty"` // for allowlist-cidr
	ProxyAddr      string   `json:"proxy_addr,omitempty"`    // for http-proxy (host:port)
}

// LabNetworkProfileGet returns the current stored lab network profile.
func (h *Handler) LabNetworkProfileGet(w http.ResponseWriter, r *http.Request) {
	s, err := h.stateMgr.GetSnapshot()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "control plane state unavailable")
		return
	}

	// Determine whether a session is currently active
	session, _ := readLabSession()
	sessionActive := session != nil && labUnitIsActive(session.UnitName)

	profile := string(s.Lab.NetworkProfile)
	if profile == "" {
		profile = string(cpstate.LabNetOffline) // default
	}

	writeJSON(w, http.StatusOK, LabNetworkProfileResponse{
		NetworkProfile: profile,
		AllowedCIDRs:   s.Lab.AllowedCIDRs,
		ProxyAddr:      s.Lab.ProxyAddr,
		SessionActive:  sessionActive,
	})
}

// LabNetworkProfileSet stores a new lab network profile in the control plane state.
//
// Guard: rejected if a session is currently active — profile changes mid-session
// would not be applied to the running container and would create operator confusion.
//
// Journal event: HISNOS_LAB_NET_PROFILE logged on success.
func (h *Handler) LabNetworkProfileSet(w http.ResponseWriter, r *http.Request) {
	if !h.requireConfirm(w, r) {
		return
	}

	var req LabNetworkProfileRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Validate profile value
	profile, err := cpstate.ParseLabNetworkProfile(req.NetworkProfile)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Profile-specific validation
	switch profile {
	case cpstate.LabNetAllowlist:
		if len(req.AllowedCIDRs) == 0 {
			writeError(w, http.StatusBadRequest,
				"allowlist-cidr profile requires at least one CIDR in allowed_cidrs")
			return
		}
		if err := validateCIDRs(req.AllowedCIDRs); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid CIDR: %s", err))
			return
		}

	case cpstate.LabNetHTTPProxy:
		if req.ProxyAddr == "" {
			writeError(w, http.StatusBadRequest,
				"http-proxy profile requires proxy_addr (host:port)")
			return
		}
		if err := validateProxyAddr(req.ProxyAddr); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid proxy_addr: %s", err))
			return
		}

	case cpstate.LabNetDNSSinkhole:
		// No extra parameters required; sinkhole binds to 10.72.0.1 automatically.

	case cpstate.LabNetOffline:
		// No parameters required.
	}

	// Guard: reject if a session is active
	session, _ := readLabSession()
	if session != nil && labUnitIsActive(session.UnitName) {
		writeErrorCode(w, http.StatusConflict,
			fmt.Sprintf("cannot change network profile while session %s is active — stop the session first",
				session.SessionID),
			string(cpstate.ErrForbiddenByState))
		return
	}

	// Persist to control plane state
	facts := cpstate.LabFacts{
		NetworkProfile: profile,
		AllowedCIDRs:   req.AllowedCIDRs,
		ProxyAddr:      req.ProxyAddr,
	}
	if err := h.stateMgr.SetLabFacts(facts, "lab_network_profile_set", map[string]any{
		"profile":    string(profile),
		"cidr_count": len(req.AllowedCIDRs),
		"has_proxy":  req.ProxyAddr != "",
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to persist network profile")
		return
	}

	// Journal structured event
	_ = execLogLabNetProfile(string(profile), session)

	writeJSON(w, http.StatusOK, map[string]any{
		"success":         true,
		"network_profile": string(profile),
		"allowed_cidrs":   req.AllowedCIDRs,
		"proxy_addr":      req.ProxyAddr,
	})
}

// ── Validation helpers ────────────────────────────────────────────────────────

// validateCIDRs checks that each element is a valid CIDR notation address.
func validateCIDRs(cidrs []string) error {
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			return fmt.Errorf("empty CIDR entry")
		}
		// Accept both bare IPs and CIDR notation
		if strings.Contains(cidr, "/") {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("%q: %w", cidr, err)
			}
		} else {
			if net.ParseIP(cidr) == nil {
				return fmt.Errorf("%q: not a valid IP or CIDR", cidr)
			}
		}
	}
	return nil
}

// validateProxyAddr checks that proxy_addr is a valid host:port string.
func validateProxyAddr(addr string) error {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port format: %w", err)
	}
	if host == "" {
		return fmt.Errorf("host part is empty")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be 1–65535, got %q", portStr)
	}
	return nil
}

// execLogLabNetProfile logs a HISNOS_LAB_NET_PROFILE journal event via logger(1).
func execLogLabNetProfile(profile string, session *LabSession) {
	sessionID := "none"
	if session != nil {
		sessionID = session.SessionID
	}
	_ = execpkg.Run(
		[]string{labLoggerBin, "-t", "hisnos-lab", "-p", "user.notice",
			fmt.Sprintf("HISNOS_LAB_NET_PROFILE profile=%s session=%s source=api",
				profile, sessionID)},
		execpkg.Options{Timeout: 3 * time.Second},
	)
}
