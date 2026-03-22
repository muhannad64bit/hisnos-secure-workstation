"""
commandcenter/ipc/client.py — Python IPC client for hisnos-search.sock

Wraps the line-delimited JSON Unix socket protocol used by searchd.
Used by hisnos-search-ui.py (via import) and the CLI helper.

Protocol:
  Request:  {"id": <int>, "cmd": "<cmd>", ...fields}
  Response: {"id": <int>, "ok": true/false, "results": [...], "data": ..., "error": "..."}

Thread safety: SearchClient uses a threading.Lock around socket I/O.
Re-connects automatically on broken pipe.
"""

from __future__ import annotations

import json
import os
import socket
import threading
import time
from typing import Any


def default_socket_path() -> str:
    if path := os.environ.get("HISNOS_SEARCH_SOCKET"):
        return path
    runtime = os.environ.get("XDG_RUNTIME_DIR", f"/run/user/{os.getuid()}")
    return os.path.join(runtime, "hisnos-search.sock")


class SearchError(Exception):
    """Raised when searchd returns ok=false or connection fails."""


class SearchClient:
    """Thread-safe IPC client for searchd."""

    def __init__(self, socket_path: str | None = None):
        self._socket_path = socket_path or default_socket_path()
        self._lock = threading.Lock()
        self._sock: socket.socket | None = None
        self._fh = None  # buffered file handle for readline
        self._seq = 0

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def available(self) -> bool:
        """Return True if the searchd socket exists."""
        return os.path.exists(self._socket_path)

    def search(self, query: str, limit: int = 20) -> list[dict]:
        """Search the index. Returns list of Result dicts."""
        resp = self._call("search", query=query, limit=limit)
        return resp.get("results") or []

    def execute(self, action: str) -> Any:
        """Execute an action string (open:, ipc:, shell:, browse:)."""
        resp = self._call("execute", action=action)
        return resp.get("data")

    def preview(self, path: str) -> str:
        """Fetch a file preview snippet."""
        resp = self._call("preview", preview=path)
        data = resp.get("data", "")
        return str(data) if data else ""

    def status(self) -> dict:
        """Return daemon status (file count, telemetry count, socket path)."""
        resp = self._call("status")
        return resp.get("data") or {}

    # ------------------------------------------------------------------
    # Internal
    # ------------------------------------------------------------------

    def _call(self, cmd: str, **kwargs) -> dict:
        with self._lock:
            for attempt in range(2):
                try:
                    return self._send(cmd, **kwargs)
                except (OSError, BrokenPipeError, ConnectionResetError):
                    self._close()
                    if attempt == 1:
                        raise SearchError("searchd unavailable")
                    time.sleep(0.05)  # brief wait before reconnect
        return {}

    def _send(self, cmd: str, **kwargs) -> dict:
        self._ensure_connected()
        self._seq += 1
        req = {"id": self._seq, "cmd": cmd, **kwargs}
        line = json.dumps(req) + "\n"
        self._sock.sendall(line.encode())
        raw = self._fh.readline()
        if not raw:
            raise ConnectionResetError("searchd closed connection")
        resp = json.loads(raw)
        if not resp.get("ok"):
            raise SearchError(resp.get("error", "unknown error"))
        return resp

    def _ensure_connected(self):
        if self._sock is not None:
            return
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(3.0)
        try:
            s.connect(self._socket_path)
        except OSError as e:
            s.close()
            raise SearchError(f"connect {self._socket_path}: {e}") from e
        s.settimeout(None)
        self._sock = s
        self._fh = s.makefile("r", encoding="utf-8", newline="\n")

    def _close(self):
        if self._fh:
            try:
                self._fh.close()
            except OSError:
                pass
            self._fh = None
        if self._sock:
            try:
                self._sock.close()
            except OSError:
                pass
            self._sock = None

    def close(self):
        with self._lock:
            self._close()

    def __enter__(self):
        return self

    def __exit__(self, *_):
        self.close()


# ------------------------------------------------------------------
# CLI helper (python3 client.py search "vault")
# ------------------------------------------------------------------

if __name__ == "__main__":
    import sys

    if len(sys.argv) < 2:
        print("usage: client.py <search|status|execute> [args...]", file=sys.stderr)
        sys.exit(1)

    client = SearchClient()
    if not client.available():
        print(f"searchd socket not found: {client._socket_path}", file=sys.stderr)
        sys.exit(1)

    cmd = sys.argv[1]
    try:
        if cmd == "search":
            query = " ".join(sys.argv[2:]) if len(sys.argv) > 2 else ""
            results = client.search(query)
            for r in results:
                print(f"[{r['type']:12s}] {r['title']:40s} {r.get('subtitle','')}")
        elif cmd == "status":
            st = client.status()
            print(json.dumps(st, indent=2))
        elif cmd == "execute":
            action = sys.argv[2]
            data = client.execute(action)
            print(json.dumps(data, indent=2))
        else:
            print(f"unknown command: {cmd}", file=sys.stderr)
            sys.exit(1)
    except SearchError as e:
        print(f"error: {e}", file=sys.stderr)
        sys.exit(1)
    finally:
        client.close()
