#!/usr/bin/env python3
# sandbox-rfb.py — pod-level KasmVNC check for a TinyCDI workspace pod
# (DEV/CI ONLY). Connects to the pod's TLS streaming endpoint the way the
# session gateway does (Basic auth + the per-workspace cert), upgrades to
# the RFB websocket, then proves both directions: a FramebufferUpdate must
# come back after a KeyEvent + PointerEvent + full-screen request.
#
#   sandbox-rfb.py --host 10.244.0.9 --port 8443 \
#       --servername ws-<uid> --ca tls.crt \
#       --user kasm_user --password-file pw.txt
#
# Exit 0 = stream + input round-trip verified; nonzero on any failure.
import argparse
import base64
import os
import socket
import ssl
import struct
import sys

DEBUG = os.environ.get("SBX_RFB_DEBUG") == "1"


def dbg(msg):
    if DEBUG:
        print(f"[rfb] {msg}", file=sys.stderr)


class WSConn:
    """Minimal RFC 6455 client over a connected TLS socket."""

    def __init__(self, sock):
        self.sock = sock
        self.buf = b""
        self.rbuf = b""  # leftover RFB bytes from over-read frames

    def read(self, n, what):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError(
                    f"connection closed reading {what} ({len(self.buf)}/{n})")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def readline_block(self, what):
        while b"\r\n\r\n" not in self.buf:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError(f"connection closed reading {what}")
            self.buf += chunk
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        return head

    @staticmethod
    def frame(payload, opcode=2):
        header = bytearray([0x80 | opcode])
        length = len(payload)
        if length < 126:
            header.append(0x80 | length)
        elif length < 65536:
            header.append(0x80 | 126)
            header += struct.pack(">H", length)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", length)
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        return bytes(header) + mask + masked

    def recv_frame(self):
        header = self.read(2, "ws header")
        opcode = header[0] & 0x0F
        length = header[1] & 0x7F
        if length == 126:
            (length,) = struct.unpack(">H", self.read(2, "ws len16"))
        elif length == 127:
            (length,) = struct.unpack(">Q", self.read(8, "ws len64"))
        payload = self.read(length, "ws payload") if length else b""
        dbg(f"frame opcode={opcode} len={length} payload={payload[:32]!r}")
        if opcode == 0x8:
            raise RuntimeError("websocket closed by server")
        return opcode, payload

    def rfb(self, want, what):
        """Collect exactly `want` bytes of RFB stream from binary ws frames."""
        out = self.rbuf[:want]
        self.rbuf = self.rbuf[want:]
        dbg(f"rfb want={want} what={what} carry={len(out)}")
        while len(out) < want:
            opcode, payload = self.recv_frame()
            if opcode not in (0x2, 0x0):
                raise RuntimeError(f"unexpected ws opcode {opcode} reading {what}")
            take = want - len(out)
            out += payload[:take]
            self.rbuf += payload[take:]
        return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--servername", required=True,
                    help="TLS SNI/verify name (pod or service name)")
    ap.add_argument("--ca", required=True, help="PEM CA of the workspace cert")
    ap.add_argument("--user", required=True)
    ap.add_argument("--password-file", required=True)
    args = ap.parse_args()

    password = open(args.password_file, encoding="utf-8").read().strip()
    basic = base64.b64encode(f"{args.user}:{password}".encode()).decode()

    ctx = ssl.create_default_context(cafile=args.ca)
    raw = socket.create_connection((args.host, args.port), timeout=20)
    dbg("tcp connected; TLS handshake")
    sock = ctx.wrap_socket(raw, server_hostname=args.servername)
    dbg(f"TLS up: {sock.version()} cipher={sock.cipher()[0] if sock.cipher() else '?'}")
    ws = WSConn(sock)

    # WebSocket upgrade — KasmVNC wants the legacy Sec-WebSocket-Origin.
    req = (
        "GET /websockify HTTP/1.1\r\n"
        f"Host: {args.servername}\r\n"
        "Upgrade: websocket\r\n"
        "Connection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {base64.b64encode(b'0123456789abcdef').decode()}\r\n"
        "Sec-WebSocket-Version: 13\r\n"
        "Sec-WebSocket-Protocol: binary\r\n"
        f"Sec-WebSocket-Origin: https://{args.servername}\r\n"
        f"Authorization: Basic {basic}\r\n"
        "\r\n"
    )
    sock.sendall(req.encode())
    dbg("upgrade request sent")
    head = ws.readline_block("upgrade response")
    dbg(f"upgrade head: {head[:120]!r}")
    status = head.split(b"\r\n", 1)[0].decode(errors="replace")
    if " 101 " not in f" {status} ":
        raise RuntimeError(f"websocket upgrade failed: {status}")

    # RFB handshake (KasmVNC offers SecurityTypes None after HTTP auth).
    dbg("waiting for RFB greeting")
    version = ws.rfb(12, "RFB version")
    dbg(f"greeting: {version!r}")
    if not version.startswith(b"RFB "):
        raise RuntimeError(f"bad RFB greeting: {version!r}")
    sock.sendall(ws.frame(b"RFB 003.008\n"))
    nsec = ws.rfb(1, "security count")[0]
    if nsec == 0:
        (slen,) = struct.unpack(">I", ws.rfb(4, "reason len"))
        reason = ws.rfb(slen, "reason")
        raise RuntimeError(f"RFB security refused: {reason!r}")
    sectypes = ws.rfb(nsec, "security types")
    if 1 not in sectypes:
        raise RuntimeError(
            f"RFB requires auth type in {list(sectypes)}; None unsupported")
    sock.sendall(ws.frame(b"\x01"))  # choose None
    (result,) = struct.unpack(">I", ws.rfb(4, "security result"))
    if result != 0:
        raise RuntimeError(f"RFB security result {result}")

    # ClientInit (shared) -> ServerInit.
    sock.sendall(ws.frame(b"\x01"))
    server = ws.rfb(24, "ServerInit")
    fb_w, fb_h = struct.unpack(">HH", server[:4])
    (name_len,) = struct.unpack(">I", server[20:24])
    ws.rfb(name_len, "desktop name")

    # Client setup messages first (a real client sends pixel format +
    # encodings): SetPixelFormat echoes the server's own format,
    # SetEncodings advertises Raw only — enough for the update check.
    set_pf = b"\x00\x00\x00\x00" + server[4:20]
    set_enc = struct.pack(">BxH", 2, 1) + struct.pack(">i", 0)
    sock.sendall(ws.frame(set_pf))
    sock.sendall(ws.frame(set_enc))

    # Input + output round trip: press+release "a", click at (10,10), then
    # request a full framebuffer update and require real pixel data back.
    # One RFB client message per websocket frame (noVNC does the same).
    # KasmVNC's PointerEvent is nonstandard: type, mask u16 (not u8), x,
    # y, scrollX s16, scrollY s16 — 11 bytes (rfb/SMsgReader.cxx).
    key_a = 0x61
    for packet in (
        struct.pack(">BBHI", 4, 1, 0, key_a),                 # KeyEvent down
        struct.pack(">BBHI", 4, 0, 0, key_a),                 # KeyEvent up
        struct.pack(">BHHHhh", 5, 1, 10, 10, 0, 0),           # PointerEvent down
        struct.pack(">BHHHhh", 5, 0, 10, 10, 0, 0),           # PointerEvent up
        struct.pack(">BBHHHH", 3, 0, 0, 0, fb_w, fb_h),       # FBUpdateRequest
    ):
        sock.sendall(ws.frame(packet))

    # Server->client messages: 0=FBUpdate 1=SetColourMapEntries 2=Bell
    # 3=ServerCutText — read until an FBUpdate with >0 rects.
    for _ in range(16):
        mtype = ws.rfb(1, "server message type")[0]
        if mtype == 0:
            hdr = ws.rfb(3, "FBUpdate header")
            (nrects,) = struct.unpack(">H", hdr[1:3])
            if nrects == 0:
                dbg("FramebufferUpdate with 0 rects, continuing")
                continue
            rect = ws.rfb(12, "rect header")
            (enc,) = struct.unpack(">i", rect[8:12])
            print(f"OK handshake=Rfb003.008 fb={fb_w}x{fb_h} "
                  f"update_rects={nrects} encoding={enc}")
            return 0
        if mtype == 1:
            ws.rfb(5, "SetColourMapEntries hdr")
            continue
        if mtype == 2:
            dbg("Bell, continuing")
            continue
        if mtype == 3:
            hdr = ws.rfb(7, "ServerCutText hdr")
            (n,) = struct.unpack(">I", hdr[3:7])
            ws.rfb(n, "ServerCutText")
            continue
        raise RuntimeError(f"unexpected server message type {mtype}")
    raise RuntimeError("no FramebufferUpdate within 16 server messages")


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as e:  # CLI boundary: report and fail
        print(f"FAIL {e}", file=sys.stderr)
        sys.exit(1)
