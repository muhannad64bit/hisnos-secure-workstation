/* installer/calamares/branding/hisnos/show.qml
 * Calamares installation slideshow — HisnOS branding.
 * Displayed during the exec phase while files are being written.
 * Uses Calamares SlideshowAPI v2.
 */

import QtQuick 2.15
import QtQuick.Controls 2.15
import Calamares.Slideshow 1.0

Presentation {
    id: presentation

    // Auto-advance every 6 seconds.
    Timer {
        interval: 6000
        running:  presentation.activatedInCalamares
        repeat:   true
        onTriggered: presentation.goToNextSlide()
    }

    // ── Slide 1: Welcome ──────────────────────────────────────────────────
    Slide {
        Rectangle {
            anchors.fill: parent
            color: "#0a0a14"

            Column {
                anchors.centerIn: parent
                spacing: 20

                Text {
                    anchors.horizontalCenter: parent.horizontalCenter
                    text: "HisnOS"
                    font.pixelSize: 48
                    font.bold: true
                    color: "#00c8ff"
                }

                Text {
                    anchors.horizontalCenter: parent.horizontalCenter
                    text: "SECURE WORKSTATION"
                    font.pixelSize: 14
                    letterSpacing: 3
                    color: "#556677"
                }

                Rectangle { height: 24; width: 1; color: "transparent" }

                Text {
                    anchors.horizontalCenter: parent.horizontalCenter
                    text: "Installing your hardened Fedora Kinoite workstation…"
                    font.pixelSize: 15
                    color: "#8892b0"
                }
            }
        }
    }

    // ── Slide 2: Vault ────────────────────────────────────────────────────
    Slide {
        Rectangle {
            anchors.fill: parent
            color: "#0a0a14"

            Column {
                anchors.centerIn: parent
                spacing: 16
                width: 460

                Text {
                    text: "🔒  Encrypted Vault"
                    font.pixelSize: 26
                    font.bold: true
                    color: "#ccd6f6"
                }

                Text {
                    width: parent.width
                    text: "Your personal files are protected by a gocryptfs vault "
                        + "using AES-256-GCM.  Only your passphrase can unlock it — "
                        + "even physical access to the drive doesn't expose your data."
                    font.pixelSize: 14
                    color: "#8892b0"
                    wrapMode: Text.WordWrap
                    lineHeight: 1.5
                }
            }
        }
    }

    // ── Slide 3: Firewall ─────────────────────────────────────────────────
    Slide {
        Rectangle {
            anchors.fill: parent
            color: "#0a0a14"

            Column {
                anchors.centerIn: parent
                spacing: 16
                width: 460

                Text {
                    text: "🛡  Default-Deny Egress"
                    font.pixelSize: 26
                    font.bold: true
                    color: "#ccd6f6"
                }

                Text {
                    width: parent.width
                    text: "nftables blocks all outbound traffic by default.  "
                        + "OpenSnitch lets you approve applications per-connection.  "
                        + "Choose Strict, Balanced, or Gaming-Ready in the setup wizard."
                    font.pixelSize: 14
                    color: "#8892b0"
                    wrapMode: Text.WordWrap
                    lineHeight: 1.5
                }
            }
        }
    }

    // ── Slide 4: Threat Engine ────────────────────────────────────────────
    Slide {
        Rectangle {
            anchors.fill: parent
            color: "#0a0a14"

            Column {
                anchors.centerIn: parent
                spacing: 16
                width: 460

                Text {
                    text: "⚠  Threat Engine"
                    font.pixelSize: 26
                    font.bold: true
                    color: "#ccd6f6"
                }

                Text {
                    width: parent.width
                    text: "hisnos-threatd watches process behaviour, open ports, "
                        + "and audit logs.  Anomalies surface as desktop notifications "
                        + "and appear in the governance dashboard."
                    font.pixelSize: 14
                    color: "#8892b0"
                    wrapMode: Text.WordWrap
                    lineHeight: 1.5
                }
            }
        }
    }

    // ── Slide 5: Gaming ───────────────────────────────────────────────────
    Slide {
        Rectangle {
            anchors.fill: parent
            color: "#0a0a14"

            Column {
                anchors.centerIn: parent
                spacing: 16
                width: 460

                Text {
                    text: "🎮  Gaming Without Compromise"
                    font.pixelSize: 26
                    font.bold: true
                    color: "#ccd6f6"
                }

                Text {
                    width: parent.width
                    text: "hispowerd automatically detects Steam and Proton sessions, "
                        + "isolates game threads onto dedicated cores, tunes IRQ affinity, "
                        + "and activates an nftables fast path — all without weakening "
                        + "your security posture."
                    font.pixelSize: 14
                    color: "#8892b0"
                    wrapMode: Text.WordWrap
                    lineHeight: 1.5
                }
            }
        }
    }
}
