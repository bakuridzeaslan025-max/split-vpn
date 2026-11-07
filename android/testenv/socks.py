#!/usr/bin/env python3
"""SOCKS5 for the VK channel's emulator test: stands where the far end of the
call would be. Takes any user/password, as vkfallback offers only that method.
One line per CONNECT goes to the log, which control.py serves to the test."""
import socket, socketserver, struct, sys, threading, time

LOG = sys.argv[2]
lock = threading.Lock()

def note(line):
    with lock, open(LOG, "a") as f:
        f.write(f"{time.strftime('%H:%M:%S')} {line}\n")

def read(c, n):
    b = b""
    while len(b) < n:
        chunk = c.recv(n - len(b))
        if not chunk:
            raise EOFError
        b += chunk
    return b

def pump(src, dst):
    try:
        while data := src.recv(65536):
            dst.sendall(data)
        dst.shutdown(socket.SHUT_WR)
    except OSError:
        pass

class H(socketserver.BaseRequestHandler):
    def handle(self):
        c = self.request
        try:
            self.serve(c)
        except (EOFError, OSError):
            pass  # vkfake's readiness check connects and leaves

    def serve(self, c):
        _, n = read(c, 2)
        if 2 not in read(c, n):
            c.sendall(b"\x05\xff")
            note("refused: no user/password method")
            return
        c.sendall(b"\x05\x02")
        read(c, 1)
        user = read(c, read(c, 1)[0])
        pw = read(c, read(c, 1)[0])
        c.sendall(b"\x01\x00" if user and pw else b"\x01\x01")
        _, cmd, _, atyp = read(c, 4)
        if atyp == 1:
            host = socket.inet_ntoa(read(c, 4))
        elif atyp == 3:
            host = read(c, read(c, 1)[0]).decode()
        else:
            host = socket.inet_ntop(socket.AF_INET6, read(c, 16))
        port = struct.unpack("!H", read(c, 2))[0]
        if cmd != 1:
            c.sendall(b"\x05\x07\x00\x01\0\0\0\0\0\0")
            return
        try:
            out = socket.create_connection((host, port), timeout=10)
        except OSError as e:
            note(f"fail {host}:{port} {e}")
            c.sendall(b"\x05\x05\x00\x01\0\0\0\0\0\0")
            return
        note(f"connect {host}:{port}")
        c.sendall(b"\x05\x00\x00\x01\0\0\0\0\0\0")
        with out:
            t = threading.Thread(target=pump, args=(out, c), daemon=True)
            t.start()
            pump(c, out)
            t.join()

class S(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

open(LOG, "w").close()
S(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
