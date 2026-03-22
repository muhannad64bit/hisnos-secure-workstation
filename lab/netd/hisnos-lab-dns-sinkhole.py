#!/usr/bin/env python3
# lab/netd/hisnos-lab-dns-sinkhole.py — Minimal DNS sinkhole for lab sessions
#
# Binds to <LAB_HOST_IP>:53 (default 10.72.0.1) and responds to all DNS
# queries with NXDOMAIN. Logs each query to the system journal via syslog.
#
# Purpose: allow the lab container's DNS stack to function (no timeouts),
# but return empty/negative results for every hostname. This lets malware
# samples attempt DNS resolution; the analyst can observe what names are
# queried in the journal without any real connectivity.
#
# Usage (called by hisnos-lab-netd.sh):
#   python3 hisnos-lab-dns-sinkhole.py --bind 10.72.0.1 --sid <session_id>
#
# Exits when: SIGTERM/SIGINT, or when the bind address disappears.
# Journal tag: hisnos-lab-dns (syslog identifier)

import argparse
import logging
import logging.handlers
import signal
import socket
import struct
import sys
import time


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description="HisnOS lab DNS sinkhole")
    p.add_argument("--bind", default="10.72.0.1", help="IP to bind on")
    p.add_argument("--port", type=int, default=53, help="UDP port (default 53)")
    p.add_argument("--sid", default="unknown", help="Session ID for log tagging")
    return p.parse_args()


def setup_logger(sid: str) -> logging.Logger:
    log = logging.getLogger("hisnos-lab-dns")
    log.setLevel(logging.INFO)
    handler = logging.handlers.SysLogHandler(address="/dev/log")
    handler.ident = "hisnos-lab-dns"
    fmt = logging.Formatter(f"session={sid} %(message)s")
    handler.setFormatter(fmt)
    log.addHandler(handler)
    return log


def parse_dns_qname(payload: bytes) -> str:
    """Extract the first QNAME from a DNS query payload (skips 12-byte header)."""
    try:
        pos = 12
        labels = []
        while pos < len(payload):
            length = payload[pos]
            if length == 0:
                break
            pos += 1
            labels.append(payload[pos:pos + length].decode("ascii", errors="replace"))
            pos += length
        return ".".join(labels) if labels else "<empty>"
    except Exception:
        return "<parse-error>"


def make_nxdomain(query: bytes) -> bytes:
    """Build an NXDOMAIN response mirroring the transaction ID and question."""
    if len(query) < 12:
        return b""
    txid = query[:2]
    # Flags: QR=1 (response), OPCODE=0 (standard), AA=0, TC=0, RD=1 (mirror),
    #        RA=0, Z=0, RCODE=3 (NXDOMAIN)
    flags = struct.pack(">H", 0x8183)
    qdcount = query[4:6]  # mirror question count
    ancount = b"\x00\x00"
    nscount = b"\x00\x00"
    arcount = b"\x00\x00"
    question = query[12:]  # mirror full question section
    return txid + flags + qdcount + ancount + nscount + arcount + question


def run_sinkhole(bind_ip: str, port: int, sid: str, log: logging.Logger) -> None:
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.settimeout(2.0)

    try:
        sock.bind((bind_ip, port))
    except PermissionError:
        log.error(f"HISNOS_LAB_DNS_SINKHOLE_BIND_FAILED bind={bind_ip}:{port} — need CAP_NET_BIND_SERVICE or run as root")
        sys.exit(1)
    except OSError as e:
        log.error(f"HISNOS_LAB_DNS_SINKHOLE_BIND_FAILED bind={bind_ip}:{port} err={e}")
        sys.exit(1)

    log.info(f"HISNOS_LAB_DNS_SINKHOLE_STARTED bind={bind_ip}:{port}")

    def _shutdown(signum, frame):
        log.info(f"HISNOS_LAB_DNS_SINKHOLE_STOPPED signal={signum}")
        sock.close()
        sys.exit(0)

    signal.signal(signal.SIGTERM, _shutdown)
    signal.signal(signal.SIGINT, _shutdown)

    while True:
        try:
            data, addr = sock.recvfrom(512)
        except socket.timeout:
            continue
        except OSError:
            break

        qname = parse_dns_qname(data)
        log.info(f"HISNOS_LAB_DNS_QUERY qname={qname} src={addr[0]}:{addr[1]}")

        response = make_nxdomain(data)
        if response:
            try:
                sock.sendto(response, addr)
            except OSError:
                pass

    sock.close()


def main() -> None:
    args = parse_args()
    log = setup_logger(args.sid)
    run_sinkhole(args.bind, args.port, args.sid, log)


if __name__ == "__main__":
    main()
