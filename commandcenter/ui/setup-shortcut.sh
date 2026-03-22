#!/usr/bin/env bash
# commandcenter/ui/setup-shortcut.sh
#
# Registers SUPER+SPACE → hisnos-search-toggle as a KDE KGlobalAccel shortcut.
# Also installs a .desktop file and a toggle-helper script.
#
# Usage: bash setup-shortcut.sh [--remove]
#
# Approach:
#   1. Install hisnos-search-toggle.sh → ~/.local/bin/
#   2. Install hisnos-search.desktop  → ~/.local/share/applications/
#   3. Register shortcut via kwriteconfig5 + kglobalaccel5 (KDE 5/6 compatible)
#   4. Optionally bind via dbus-send to KGlobalAccelD if running
#
# The toggle script writes "toggle" to the control socket
# (/run/user/$UID/hisnos-search-ui.sock) which the UI daemon reads.
#
# On first launch (socket missing), it starts the UI service instead.

set -euo pipefail

RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
UI_SOCK="${RUNTIME_DIR}/hisnos-search-ui.sock"
LOCAL_BIN="${HOME}/.local/bin"
LOCAL_APPS="${HOME}/.local/share/applications"
LOCAL_SHARE="${HOME}/.local/share/hisnos"

# ---------------------------------------------------------------------------
install_toggle_script() {
    mkdir -p "${LOCAL_BIN}"
    cat > "${LOCAL_BIN}/hisnos-search-toggle" << 'TOGGLE_EOF'
#!/usr/bin/env bash
# hisnos-search-toggle — send toggle to UI control socket, or start the service
RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
UI_SOCK="${RUNTIME_DIR}/hisnos-search-ui.sock"

if [[ -S "${UI_SOCK}" ]]; then
    echo -n "toggle" | socat - UNIX-CONNECT:"${UI_SOCK}" 2>/dev/null || true
else
    # Socket missing — start the service (daemon will show on start via --show)
    systemctl --user start hisnos-search-ui.service
fi
TOGGLE_EOF
    chmod +x "${LOCAL_BIN}/hisnos-search-toggle"
    echo "[setup] installed ${LOCAL_BIN}/hisnos-search-toggle"
}

# ---------------------------------------------------------------------------
install_desktop_file() {
    mkdir -p "${LOCAL_APPS}"
    cat > "${LOCAL_APPS}/hisnos-search.desktop" << DESKTOP_EOF
[Desktop Entry]
Name=HisnOS Command Center
Comment=Search files, commands, and security events
Exec=${LOCAL_BIN}/hisnos-search-toggle
Icon=system-search
Type=Application
Categories=System;Security;
Keywords=search;files;commands;security;vault;firewall;
X-KDE-GlobalAccel=hisnos-search
NoDisplay=false
DESKTOP_EOF
    echo "[setup] installed ${LOCAL_APPS}/hisnos-search.desktop"
    update-desktop-database "${LOCAL_APPS}" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
register_kde_shortcut() {
    # KDE 5: kwriteconfig5; KDE 6: kwriteconfig6 (same syntax, try both)
    local KWRITE
    if command -v kwriteconfig6 &>/dev/null; then
        KWRITE=kwriteconfig6
    elif command -v kwriteconfig5 &>/dev/null; then
        KWRITE=kwriteconfig5
    else
        echo "[setup] WARNING: kwriteconfig5/6 not found — shortcut not registered in KDE config"
        echo "[setup] To manually add: System Settings → Shortcuts → Custom Shortcuts → add SUPER+SPACE → ${LOCAL_BIN}/hisnos-search-toggle"
        return
    fi

    # Register the shortcut in kglobalshortcutsrc.
    # Component = khotkeys, group = hisnos-search
    "${KWRITE}" --file kglobalshortcutsrc \
        --group "hisnos-search.desktop" \
        --key "_launch" \
        "Meta+Space,none,HisnOS Command Center"

    echo "[setup] registered SUPER+SPACE in kglobalshortcutsrc"

    # Reload KDE global shortcuts daemon if running.
    if command -v kglobalaccel5 &>/dev/null; then
        dbus-send --session --type=method_call \
            --dest=org.kde.kglobalaccel \
            /kglobalaccel \
            org.kde.KGlobalAccel.reloadShortcuts 2>/dev/null && \
            echo "[setup] reloaded KDE global shortcuts" || true
    fi

    # Alternative: use qdbus if available.
    if command -v qdbus6 &>/dev/null; then
        qdbus6 org.kde.kglobalaccel /kglobalaccel reloadShortcuts 2>/dev/null || true
    elif command -v qdbus &>/dev/null; then
        qdbus org.kde.kglobalaccel /kglobalaccel reloadShortcuts 2>/dev/null || true
    fi
}

# ---------------------------------------------------------------------------
remove_shortcut() {
    echo "[setup] removing HisnOS shortcut registration..."
    rm -f "${LOCAL_BIN}/hisnos-search-toggle"
    rm -f "${LOCAL_APPS}/hisnos-search.desktop"

    local KWRITE
    if command -v kwriteconfig6 &>/dev/null; then
        KWRITE=kwriteconfig6
    elif command -v kwriteconfig5 &>/dev/null; then
        KWRITE=kwriteconfig5
    fi
    if [[ -n "${KWRITE:-}" ]]; then
        "${KWRITE}" --file kglobalshortcutsrc \
            --group "hisnos-search.desktop" \
            --key "_launch" "none,none,HisnOS Command Center"
    fi
    echo "[setup] done"
}

# ---------------------------------------------------------------------------
main() {
    if [[ "${1:-}" == "--remove" ]]; then
        remove_shortcut
        exit 0
    fi

    echo "[setup] Installing HisnOS Command Center shortcut (SUPER+SPACE)..."
    install_toggle_script
    install_desktop_file
    register_kde_shortcut

    echo ""
    echo "[setup] Done. Summary:"
    echo "  Toggle script:  ${LOCAL_BIN}/hisnos-search-toggle"
    echo "  Desktop file:   ${LOCAL_APPS}/hisnos-search.desktop"
    echo "  Shortcut:       SUPER+SPACE"
    echo ""
    echo "  If SUPER+SPACE doesn't work after login, open:"
    echo "  System Settings → Shortcuts → Custom Shortcuts"
    echo "  and manually bind SUPER+SPACE to: ${LOCAL_BIN}/hisnos-search-toggle"
    echo ""
    echo "  Requires: socat (dnf install socat)"
}

main "$@"
