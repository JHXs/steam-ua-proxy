#!/usr/bin/env python3
"""原始 TCP 回显服务器，用于测试 CONNECT 盲转发隧道。"""
import socket
import sys
import threading

PORT = int(sys.argv[1])


def handle(c):
    try:
        while True:
            d = c.recv(65536)
            if not d:
                return
            c.sendall(b"ECHO:" + d)
    except OSError:
        return
    finally:
        try:
            c.close()
        except OSError:
            pass


srv = socket.socket()
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", PORT))
srv.listen(64)
while True:
    conn, _ = srv.accept()
    threading.Thread(target=handle, args=(conn,), daemon=True).start()
