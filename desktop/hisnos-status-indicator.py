#!/usr/bin/env python3
# desktop/hisnos-status-indicator.py
#
# HisnOS system tray status indicator.
# Shows: risk level (colour), vault state, gaming mode state.
#
# Dependencies: python3-pyside6 (or PyQt6 as fallback)
# Install:  dnf install python3-pyside6
#
# The indicator reads JSON state files rather than connecting to daemons
# directly — this makes it robust even when daemons are temporarily stopped.
#
# State files polled every 10 seconds:
#   /var/lib/hisnos/threat-state.json     — risk score + level
#   /var/lib/hisnos/gaming-state.json     — gaming mode active flag
#   /var/lib/hisnos/boot-health.json      — last boot health
#
# Vault state is detected via /proc/mounts (gocryptfs presence).

import json
import os
import sys
import subprocess
import signal
from pathlib import Path

try:
    from PySide6.QtWidgets import (
        QApplication, QSystemTrayIcon, QMenu, QAction,
    )
    from PySide6.QtGui import QIcon, QColor, QPixmap, QPainter
    from PySide6.QtCore import QTimer, Qt
except ImportError:
    try:
        from PyQt6.QtWidgets import (
            QApplication, QSystemTrayIcon, QMenu, QAction,
        )
        from PyQt6.QtGui import QIcon, QColor, QPixmap, QPainter
        from PyQt6.QtCore import QTimer, Qt
    except ImportError:
        print("ERROR: PySide6 or PyQt6 required. Install: dnf install python3-pyside6",
              file=sys.stderr)
        sys.exit(1)

# ── Constants ────────────────────────────────────────────────────────────────
POLL_INTERVAL_MS = 10_000  # 10 seconds
STATE_DIR = Path("/var/lib/hisnos")

RISK_COLOURS = {
    "critical": "#ff4444",
    "high":     "#ff8800",
    "medium":   "#ffcc00",
    "low":      "#44cc44",
    "minimal":  "#00c8ff",
    "unknown":  "#556677",
}


# ── Helpers ──────────────────────────────────────────────────────────────────

def read_json(path: Path) -> dict:
    try:
        return json.loads(path.read_text())
    except Exception:
        return {}


def vault_mounted() -> bool:
    try:
        mounts = Path("/proc/mounts").read_text()
        return "gocryptfs" in mounts or "fuse.gocryptfs" in mounts
    except Exception:
        return False


def make_circle_icon(hex_colour: str, size: int = 22) -> QIcon:
    """Render a filled circle of the given colour as a QIcon."""
    px = QPixmap(size, size)
    px.fill(Qt.transparent)
    painter = QPainter(px)
    painter.setRenderHint(QPainter.Antialiasing)
    colour = QColor(hex_colour)
    painter.setBrush(colour)
    painter.setPen(Qt.NoPen)
    margin = 2
    painter.drawEllipse(margin, margin, size - 2 * margin, size - 2 * margin)
    painter.end()
    return QIcon(px)


# ── State reader ─────────────────────────────────────────────────────────────

class HisnOSState:
    def __init__(self):
        self.risk_level   = "unknown"
        self.risk_score   = 0
        self.vault        = False
        self.gaming       = False
        self.boot_ok      = True
        self.failed_units = 0

    def refresh(self):
        threat = read_json(STATE_DIR / "threat-state.json")
        gaming = read_json(STATE_DIR / "gaming-state.json")
        boot   = read_json(STATE_DIR / "boot-health.json")

        self.risk_level   = threat.get("risk_level", "unknown").lower()
        self.risk_score   = threat.get("risk_score", 0)
        self.gaming       = gaming.get("gaming_active", False)
        self.vault        = vault_mounted()
        self.boot_ok      = boot.get("last_boot_successful", True)
        self.failed_units = boot.get("failed_units_count", 0)

    def icon_colour(self) -> str:
        return RISK_COLOURS.get(self.risk_level, RISK_COLOURS["unknown"])

    def tooltip(self) -> str:
        vault_str  = "Unlocked" if self.vault  else "Locked"
        gaming_str = "Active"   if self.gaming else "Idle"
        boot_str   = "OK" if self.boot_ok else f"WARNING ({self.failed_units} failed units)"
        return (
            f"HisnOS\n"
            f"Risk: {self.risk_level.title()} ({self.risk_score})\n"
            f"Vault: {vault_str}\n"
            f"Gaming: {gaming_str}\n"
            f"Last boot: {boot_str}"
        )


# ── Tray indicator ───────────────────────────────────────────────────────────

class StatusIndicator:
    def __init__(self, app: QApplication):
        self.app   = app
        self.state = HisnOSState()

        # Build tray icon.
        self.tray = QSystemTrayIcon(app)
        self.tray.setVisible(True)

        # Context menu.
        self.menu = QMenu()
        self._build_menu()
        self.tray.setContextMenu(self.menu)

        # Initial refresh.
        self._refresh()

        # Poll timer.
        self.timer = QTimer()
        self.timer.setInterval(POLL_INTERVAL_MS)
        self.timer.timeout.connect(self._refresh)
        self.timer.start()

        # Handle SIGTERM gracefully.
        signal.signal(signal.SIGTERM, lambda *_: self.app.quit())

    def _build_menu(self):
        self.menu.clear()

        # Status label (non-interactive).
        self._status_action = QAction("HisnOS Status", self.menu)
        self._status_action.setEnabled(False)
        self.menu.addAction(self._status_action)

        self.menu.addSeparator()

        # Open Governance Dashboard.
        dashboard_action = QAction("Open Dashboard", self.menu)
        dashboard_action.triggered.connect(self._open_dashboard)
        self.menu.addAction(dashboard_action)

        # Open Search overlay.
        search_action = QAction("Command Search (SUPER+SPACE)", self.menu)
        search_action.triggered.connect(self._open_search)
        self.menu.addAction(search_action)

        self.menu.addSeparator()

        # Vault actions.
        self._vault_action = QAction("Vault: …", self.menu)
        self._vault_action.triggered.connect(self._toggle_vault)
        self.menu.addAction(self._vault_action)

        self.menu.addSeparator()

        # Quit.
        quit_action = QAction("Quit Indicator", self.menu)
        quit_action.triggered.connect(self.app.quit)
        self.menu.addAction(quit_action)

    def _refresh(self):
        self.state.refresh()
        colour = self.state.icon_colour()
        self.tray.setIcon(make_circle_icon(colour))
        self.tray.setToolTip(self.state.tooltip())

        # Update dynamic menu labels.
        risk_label = f"Risk: {self.state.risk_level.title()} ({self.state.risk_score})"
        gaming_label = "Gaming: Active" if self.state.gaming else "Gaming: Idle"
        vault_label = "Vault: Unlock…" if not self.state.vault else "Vault: Lock"
        self._status_action.setText(f"{risk_label}  |  {gaming_label}")
        self._vault_action.setText(vault_label)

        # Show notification for critical risk (once per transition).
        if self.state.risk_level == "critical" and not getattr(self, "_notified_critical", False):
            self.tray.showMessage(
                "HisnOS — Critical Threat",
                "Risk level is CRITICAL. Review the Governance Dashboard.",
                QSystemTrayIcon.Critical,
                5000,
            )
            self._notified_critical = True
        elif self.state.risk_level != "critical":
            self._notified_critical = False

    def _open_dashboard(self):
        subprocess.Popen(["xdg-open", "http://localhost:9443"], start_new_session=True)

    def _open_search(self):
        # Send toggle to the search UI control socket.
        uid = os.getuid()
        sock = f"/run/user/{uid}/hisnos-search-ui.sock"
        try:
            import socket
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
                s.connect(sock)
                s.sendall(b"toggle\n")
        except Exception:
            pass  # Search UI not running — ignore.

    def _toggle_vault(self):
        if self.state.vault:
            subprocess.Popen(["hisnos-vault", "lock"], start_new_session=True)
        else:
            # Open a simple terminal to run vault unlock interactively.
            subprocess.Popen(
                ["konsole", "-e", "hisnos-vault", "unlock"],
                start_new_session=True,
            )


# ── Entry point ──────────────────────────────────────────────────────────────

def main():
    app = QApplication(sys.argv)
    app.setQuitOnLastWindowClosed(False)
    app.setApplicationName("HisnOS Status Indicator")

    if not QSystemTrayIcon.isSystemTrayAvailable():
        print("ERROR: system tray not available. Is a desktop environment running?",
              file=sys.stderr)
        sys.exit(1)

    _indicator = StatusIndicator(app)
    sys.exit(app.exec())


if __name__ == "__main__":
    main()
