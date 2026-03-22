#!/usr/bin/env python3
"""
commandcenter/ui/hisnos-search-ui.py — HisnOS Command Center Overlay

Architecture: persistent daemon (started by systemd user service).
Show/hide via control socket: /run/user/$UID/hisnos-search-ui.sock
SUPER+SPACE shortcut sends "show" to the control socket via KDE KGlobalAccel
or a wrapper script that writes to the socket directly.

Performance targets:
  - <150ms from show command to visible window
  - <20ms search-to-results (searchd does the heavy lifting)
  - <150MB RSS (PySide6 baseline ~80MB + index data in searchd)

UI layout:
  ┌─────────────────────────────────────────────────────────────────┐
  │  [🔍  Search files, commands, events...              ]  [ESC]   │
  ├──────────────────────────────────────────────┬──────────────────┤
  │  FILES                                       │                  │
  │    📄 vault.sh          ~/hisnos/vault/      │   PREVIEW        │
  │    📁 vault/            ~/hisnos/            │                  │
  │  COMMANDS                                    │   (selected      │
  │    ⚡ Lock Vault        Immediately lock...  │    item          │
  │    ⚡ Reload Firewall   Reload nftables...   │    content)      │
  │  SECURITY EVENTS                             │                  │
  │    ⚠  vault_exposure   audit · warn          │                  │
  ├──────────────────────────────────────────────┴──────────────────┤
  │  Risk: LOW  │  Vault: MOUNTED  │  Firewall: ACTIVE  │  42 files │
  └─────────────────────────────────────────────────────────────────┘

Keyboard:
  ESC / SUPER+SPACE  — hide
  ↑ / ↓             — navigate results
  Enter             — execute selected action
  Ctrl+P            — toggle preview pane
  Tab               — cycle result groups
"""

from __future__ import annotations

import json
import os
import signal
import socket
import sys
import threading
import time
from typing import Any

# PySide6 — must be installed: pip install PySide6
from PySide6.QtCore import (
    Qt, QThread, Signal, QObject, QTimer, Slot
)
from PySide6.QtGui import (
    QFont, QKeySequence, QColor, QPalette, QShortcut
)
from PySide6.QtWidgets import (
    QApplication, QWidget, QVBoxLayout, QHBoxLayout,
    QLineEdit, QListWidget, QListWidgetItem, QLabel,
    QTextEdit, QSplitter, QFrame, QSizePolicy
)

# Adjust sys.path so we can import the IPC client.
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(_HERE, "..", "ipc"))
from client import SearchClient, SearchError  # noqa: E402

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

WINDOW_WIDTH = 860
WINDOW_HEIGHT = 540
SEARCH_DEBOUNCE_MS = 50
MAX_RESULTS = 40

RISK_COLORS = {
    "critical": "#e74c3c",
    "high":     "#e67e22",
    "medium":   "#f39c12",
    "low":      "#27ae60",
    "unknown":  "#7f8c8d",
}

GROUP_ICONS = {
    "FILE":           "📄",
    "COMMAND":        "⚡",
    "SECURITY_EVENT": "⚠ ",
    "APP":            "🖥",
}

STYLESHEET = """
QWidget {
    background-color: #1a1a2e;
    color: #e0e0e0;
    font-family: 'JetBrains Mono', 'Fira Code', monospace;
    font-size: 13px;
}
QLineEdit {
    background-color: #16213e;
    border: 1px solid #0f3460;
    border-radius: 6px;
    padding: 8px 12px;
    font-size: 15px;
    color: #ffffff;
    selection-background-color: #0f3460;
}
QLineEdit:focus {
    border: 1px solid #e94560;
}
QListWidget {
    background-color: #16213e;
    border: none;
    outline: none;
}
QListWidget::item {
    padding: 4px 8px;
    border-radius: 4px;
}
QListWidget::item:selected {
    background-color: #0f3460;
    color: #ffffff;
}
QListWidget::item:hover {
    background-color: #0d2137;
}
QTextEdit {
    background-color: #0d1117;
    border: none;
    color: #8b949e;
    font-size: 12px;
}
QLabel#statusBar {
    background-color: #0d1117;
    color: #7f8c8d;
    padding: 3px 8px;
    font-size: 11px;
    border-top: 1px solid #0f3460;
}
QLabel#groupHeader {
    color: #7f8c8d;
    font-size: 11px;
    padding: 6px 8px 2px 8px;
    font-weight: bold;
}
"""

# ---------------------------------------------------------------------------
# Search worker (runs in a QThread to avoid blocking UI)
# ---------------------------------------------------------------------------

class SearchWorker(QObject):
    results_ready = Signal(list)  # emits list of result dicts
    error = Signal(str)

    def __init__(self, client: SearchClient):
        super().__init__()
        self._client = client
        self._query = ""
        self._lock = threading.Lock()

    @Slot(str)
    def search(self, query: str):
        with self._lock:
            self._query = query
        try:
            results = self._client.search(query, MAX_RESULTS)
            self.results_ready.emit(results)
        except SearchError as e:
            self.error.emit(str(e))
        except Exception as e:
            self.error.emit(f"unexpected: {e}")


# ---------------------------------------------------------------------------
# Status bar data fetcher
# ---------------------------------------------------------------------------

class StatusFetcher(QObject):
    status_updated = Signal(dict)

    def __init__(self):
        super().__init__()

    @Slot()
    def fetch(self):
        data = {}
        # Read threat-state.json for risk level.
        try:
            with open("/var/lib/hisnos/threat-state.json") as f:
                ts = json.load(f)
            data["risk_level"] = ts.get("risk_level", "unknown")
            data["risk_score"] = ts.get("risk_score", 0)
        except OSError:
            data["risk_level"] = "unknown"
            data["risk_score"] = 0

        # Check vault mount via /proc/mounts.
        try:
            with open("/proc/mounts") as f:
                mounts = f.read()
            data["vault_mounted"] = "gocryptfs" in mounts
        except OSError:
            data["vault_mounted"] = False

        # Check nftables hisnos_egress table.
        import subprocess
        try:
            r = subprocess.run(
                ["/usr/sbin/nft", "list", "table", "inet", "hisnos_egress"],
                capture_output=True, timeout=2
            )
            data["firewall_active"] = r.returncode == 0
        except Exception:
            data["firewall_active"] = False

        self.status_updated.emit(data)


# ---------------------------------------------------------------------------
# Main overlay window
# ---------------------------------------------------------------------------

class SearchOverlay(QWidget):

    def __init__(self, client: SearchClient):
        super().__init__()
        self._client = client
        self._results: list[dict] = []
        self._show_preview = True
        self._status: dict = {}

        self._setup_window()
        self._setup_ui()
        self._setup_workers()
        self._setup_debounce()
        self._setup_status_timer()

    # -----------------------------------------------------------------------
    # Window setup
    # -----------------------------------------------------------------------

    def _setup_window(self):
        self.setWindowTitle("HisnOS Command Center")
        self.setWindowFlags(
            Qt.WindowType.FramelessWindowHint |
            Qt.WindowType.WindowStaysOnTopHint |
            Qt.WindowType.Tool
        )
        self.setAttribute(Qt.WidgetAttribute.WA_TranslucentBackground, False)
        self.setStyleSheet(STYLESHEET)
        self.resize(WINDOW_WIDTH, WINDOW_HEIGHT)
        self._centre_on_screen()

    def _centre_on_screen(self):
        screen = QApplication.primaryScreen().geometry()
        x = (screen.width() - WINDOW_WIDTH) // 2
        y = int(screen.height() * 0.2)
        self.move(x, y)

    # -----------------------------------------------------------------------
    # UI layout
    # -----------------------------------------------------------------------

    def _setup_ui(self):
        root = QVBoxLayout(self)
        root.setContentsMargins(12, 12, 12, 0)
        root.setSpacing(8)

        # Search bar row
        search_row = QHBoxLayout()
        self._search_input = QLineEdit()
        self._search_input.setPlaceholderText("Search files, commands, security events…")
        self._search_input.textChanged.connect(self._on_query_changed)
        self._search_input.installEventFilter(self)
        search_row.addWidget(self._search_input)
        root.addLayout(search_row)

        # Splitter: results list | preview pane
        splitter = QSplitter(Qt.Orientation.Horizontal)
        splitter.setHandleWidth(1)

        self._results_list = QListWidget()
        self._results_list.setFocusPolicy(Qt.FocusPolicy.NoFocus)
        self._results_list.itemActivated.connect(self._on_activate)
        self._results_list.currentRowChanged.connect(self._on_selection_changed)
        splitter.addWidget(self._results_list)

        self._preview = QTextEdit()
        self._preview.setReadOnly(True)
        self._preview.setMinimumWidth(220)
        splitter.addWidget(self._preview)
        splitter.setSizes([580, 260])
        root.addWidget(splitter, stretch=1)

        # Status bar
        self._status_label = QLabel("Connecting…")
        self._status_label.setObjectName("statusBar")
        self._status_label.setSizePolicy(QSizePolicy.Policy.Expanding, QSizePolicy.Policy.Fixed)
        root.addWidget(self._status_label)
        root.setContentsMargins(12, 12, 12, 0)

        # Keyboard shortcuts
        QShortcut(QKeySequence(Qt.Key.Key_Escape), self, activated=self.hide_overlay)
        QShortcut(QKeySequence("Ctrl+P"), self, activated=self._toggle_preview)
        QShortcut(QKeySequence(Qt.Key.Key_Return), self, activated=self._on_enter)
        QShortcut(QKeySequence(Qt.Key.Key_Down), self, activated=self._move_down)
        QShortcut(QKeySequence(Qt.Key.Key_Up), self, activated=self._move_up)

    # -----------------------------------------------------------------------
    # Workers
    # -----------------------------------------------------------------------

    def _setup_workers(self):
        # Search worker thread
        self._search_thread = QThread()
        self._search_worker = SearchWorker(self._client)
        self._search_worker.moveToThread(self._search_thread)
        self._search_worker.results_ready.connect(self._on_results)
        self._search_worker.error.connect(self._on_search_error)
        self._search_thread.start()

        # Status fetcher thread
        self._status_thread = QThread()
        self._status_fetcher = StatusFetcher()
        self._status_fetcher.moveToThread(self._status_thread)
        self._status_fetcher.status_updated.connect(self._on_status_updated)
        self._status_thread.start()

    def _setup_debounce(self):
        self._debounce_timer = QTimer()
        self._debounce_timer.setSingleShot(True)
        self._debounce_timer.setInterval(SEARCH_DEBOUNCE_MS)
        self._debounce_timer.timeout.connect(self._fire_search)

    def _setup_status_timer(self):
        self._status_timer = QTimer()
        self._status_timer.setInterval(10_000)  # refresh every 10s
        self._status_timer.timeout.connect(self._status_fetcher.fetch)
        self._status_timer.start()
        # Fetch immediately in background.
        QTimer.singleShot(200, self._status_fetcher.fetch)

    # -----------------------------------------------------------------------
    # Search flow
    # -----------------------------------------------------------------------

    def _on_query_changed(self, text: str):
        self._debounce_timer.start()

    def _fire_search(self):
        query = self._search_input.text().strip()
        if not query:
            self._results_list.clear()
            self._results = []
            self._preview.clear()
            return
        # Signal the worker via a direct call (thread-safe via GIL for this simple case).
        self._search_worker.search(query)

    @Slot(list)
    def _on_results(self, results: list):
        self._results = results
        self._populate_list(results)

    @Slot(str)
    def _on_search_error(self, error: str):
        self._status_label.setText(f"⚠  searchd: {error}")

    def _populate_list(self, results: list):
        self._results_list.clear()
        if not results:
            item = QListWidgetItem("  No results")
            item.setFlags(Qt.ItemFlag.NoItemFlags)
            self._results_list.addItem(item)
            return

        # Group by type.
        groups: dict[str, list[dict]] = {}
        for r in results:
            groups.setdefault(r["type"], []).append(r)

        type_order = ["COMMAND", "FILE", "SECURITY_EVENT", "APP"]
        for rtype in type_order:
            if rtype not in groups:
                continue
            # Group header (non-selectable).
            header = QListWidgetItem(f"  {rtype.replace('_', ' ')}")
            header.setFlags(Qt.ItemFlag.NoItemFlags)
            header.setForeground(QColor("#7f8c8d"))
            font = QFont()
            font.setBold(True)
            font.setPointSize(10)
            header.setFont(font)
            self._results_list.addItem(header)

            icon = GROUP_ICONS.get(rtype, "  ")
            for r in groups[rtype]:
                title = r.get("title", "")
                subtitle = r.get("subtitle", "")
                ts = r.get("timestamp", "")
                ts_str = f"  {ts[:16]}" if ts else ""
                text = f"  {icon} {title:<38} {subtitle[:35]}{ts_str}"
                item = QListWidgetItem(text)
                item.setData(Qt.ItemDataRole.UserRole, r)
                if r.get("risk_level") == "critical":
                    item.setForeground(QColor(RISK_COLORS["critical"]))
                elif r.get("risk_level") == "warn":
                    item.setForeground(QColor(RISK_COLORS["medium"]))
                self._results_list.addItem(item)

        # Select first real item.
        for i in range(self._results_list.count()):
            item = self._results_list.item(i)
            if item.flags() & Qt.ItemFlag.ItemIsEnabled:
                self._results_list.setCurrentRow(i)
                break

    # -----------------------------------------------------------------------
    # Selection & activation
    # -----------------------------------------------------------------------

    def _on_selection_changed(self, row: int):
        item = self._results_list.item(row)
        if item is None:
            return
        result = item.data(Qt.ItemDataRole.UserRole)
        if result is None:
            return
        self._update_preview(result)

    def _update_preview(self, result: dict):
        action = result.get("action", "")
        preview = result.get("preview", "")
        rtype = result.get("type", "")

        if preview:
            self._preview.setPlainText(preview)
            return

        if rtype == "FILE" and action.startswith("open:"):
            path = action[len("open:"):]
            try:
                text = self._client.preview(path)
                self._preview.setPlainText(text or "(binary or empty)")
            except SearchError:
                self._preview.setPlainText("(preview unavailable)")
            return

        lines = []
        lines.append(f"Title:    {result.get('title', '')}")
        lines.append(f"Type:     {rtype}")
        lines.append(f"Action:   {action}")
        if sub := result.get("subtitle"):
            lines.append(f"Source:   {sub}")
        if ts := result.get("timestamp"):
            lines.append(f"Time:     {ts}")
        if rl := result.get("risk_level"):
            lines.append(f"Risk:     {rl.upper()}")
        self._preview.setPlainText("\n".join(lines))

    def _on_activate(self, item: QListWidgetItem):
        result = item.data(Qt.ItemDataRole.UserRole)
        if result is None:
            return
        self._execute_result(result)

    def _on_enter(self):
        row = self._results_list.currentRow()
        if row < 0:
            return
        item = self._results_list.item(row)
        if item:
            self._on_activate(item)

    def _execute_result(self, result: dict):
        action = result.get("action", "")
        if not action:
            return
        try:
            self._client.execute(action)
        except SearchError as e:
            self._status_label.setText(f"⚠  {e}")
            return
        self.hide_overlay()

    def _move_down(self):
        self._move_selection(1)

    def _move_up(self):
        self._move_selection(-1)

    def _move_selection(self, delta: int):
        count = self._results_list.count()
        if count == 0:
            return
        row = self._results_list.currentRow()
        new_row = row + delta
        while 0 <= new_row < count:
            item = self._results_list.item(new_row)
            if item and (item.flags() & Qt.ItemFlag.ItemIsEnabled):
                self._results_list.setCurrentRow(new_row)
                return
            new_row += delta

    # -----------------------------------------------------------------------
    # Status bar
    # -----------------------------------------------------------------------

    @Slot(dict)
    def _on_status_updated(self, data: dict):
        self._status = data
        self._refresh_status_bar()

    def _refresh_status_bar(self):
        risk = self._status.get("risk_level", "unknown").upper()
        risk_color = RISK_COLORS.get(self._status.get("risk_level", "unknown"), RISK_COLORS["unknown"])
        vault = "MOUNTED" if self._status.get("vault_mounted") else "LOCKED"
        fw = "ACTIVE" if self._status.get("firewall_active") else "DOWN ⚠"
        score = self._status.get("risk_score", 0)

        # Build rich HTML status line.
        parts = [
            f'Risk: <span style="color:{risk_color};font-weight:bold">{risk} ({score})</span>',
            f'Vault: <b>{vault}</b>',
            f'Firewall: <b>{fw}</b>',
        ]
        try:
            st = self._client.status()
            parts.append(f'Index: {st.get("files", 0)} files / {st.get("telemetry", 0)} events')
        except Exception:
            pass
        self._status_label.setText("  " + "  │  ".join(parts))

    # -----------------------------------------------------------------------
    # Show / hide
    # -----------------------------------------------------------------------

    def show_overlay(self):
        self._centre_on_screen()
        self.show()
        self.raise_()
        self.activateWindow()
        self._search_input.clear()
        self._search_input.setFocus()
        self._results_list.clear()
        self._preview.clear()
        # Refresh status bar on every show.
        QTimer.singleShot(0, self._status_fetcher.fetch)

    def hide_overlay(self):
        self.hide()

    def _toggle_preview(self):
        self._show_preview = not self._show_preview
        if hasattr(self, "_preview"):
            self._preview.setVisible(self._show_preview)

    # -----------------------------------------------------------------------
    # Event filter (capture arrow keys in search input)
    # -----------------------------------------------------------------------

    def eventFilter(self, obj, event):
        from PySide6.QtCore import QEvent
        if obj is self._search_input and event.type() == QEvent.Type.KeyPress:
            key = event.key()
            if key == Qt.Key.Key_Down:
                self._move_down()
                return True
            if key == Qt.Key.Key_Up:
                self._move_up()
                return True
            if key == Qt.Key.Key_Return or key == Qt.Key.Key_Enter:
                self._on_enter()
                return True
        return super().eventFilter(obj, event)

    def closeEvent(self, event):
        event.ignore()
        self.hide_overlay()


# ---------------------------------------------------------------------------
# Control socket server (listens for "show" / "hide" / "toggle")
# ---------------------------------------------------------------------------

class ControlServer(threading.Thread):
    def __init__(self, socket_path: str, overlay: SearchOverlay):
        super().__init__(daemon=True, name="control-socket")
        self._socket_path = socket_path
        self._overlay = overlay

    def run(self):
        try:
            os.unlink(self._socket_path)
        except OSError:
            pass
        srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            srv.bind(self._socket_path)
            os.chmod(self._socket_path, 0o600)
            srv.listen(5)
        except OSError as e:
            print(f"[control] listen error: {e}", file=sys.stderr)
            return

        while True:
            try:
                conn, _ = srv.accept()
                with conn:
                    data = conn.recv(64).decode(errors="replace").strip()
                    self._handle(data)
            except OSError:
                break

    def _handle(self, cmd: str):
        from PySide6.QtCore import QMetaObject, Qt
        if cmd == "show":
            QMetaObject.invokeMethod(self._overlay, "show_overlay", Qt.ConnectionType.QueuedConnection)
        elif cmd == "hide":
            QMetaObject.invokeMethod(self._overlay, "hide_overlay", Qt.ConnectionType.QueuedConnection)
        elif cmd == "toggle":
            if self._overlay.isVisible():
                QMetaObject.invokeMethod(self._overlay, "hide_overlay", Qt.ConnectionType.QueuedConnection)
            else:
                QMetaObject.invokeMethod(self._overlay, "show_overlay", Qt.ConnectionType.QueuedConnection)


def control_socket_path() -> str:
    runtime = os.environ.get("XDG_RUNTIME_DIR", f"/run/user/{os.getuid()}")
    return os.path.join(runtime, "hisnos-search-ui.sock")


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

def main():
    # Ignore SIGPIPE; use SIGINT for clean exit.
    signal.signal(signal.SIGINT, signal.SIG_DFL)

    app = QApplication(sys.argv)
    app.setApplicationName("HisnOS Command Center")
    app.setQuitOnLastWindowClosed(False)

    # Build IPC client — connect lazily on first search.
    client = SearchClient()

    overlay = SearchOverlay(client)

    # Start control socket.
    ctrl_sock = control_socket_path()
    ctrl = ControlServer(ctrl_sock, overlay)
    ctrl.start()

    # Show immediately if --show passed (e.g. from shortcut script on first launch).
    if "--show" in sys.argv:
        overlay.show_overlay()

    sys.exit(app.exec())


if __name__ == "__main__":
    main()
