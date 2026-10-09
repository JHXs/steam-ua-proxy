#!/usr/bin/env python3
"""UA 回显 HTTP 服务器（支持 keep-alive、chunked、Expect），用于对比代理行为。"""
import socket
import sys
import threading

PORT = int(sys.argv[1])
conns = 0
lock = threading.Lock()


def read_head(buf, c):
    while b"\r\n\r\n" not in buf:
        d = c.recv(65536)
        if not d:
            return None, b""
        buf += d
    head, buf = buf.split(b"\r\n\r\n", 1)
    return head, buf


def read_body(c, buf, hdrs):
    body = b""
    te = hdrs.get(b"transfer-encoding", b"").lower()
    if b"chunked" in te:
        while True:
            while b"\r\n" not in buf:
                d = c.recv(65536)
                if not d:
                    return body, buf
                buf += d
            line, buf = buf.split(b"\r\n", 1)
            size = int(line.split(b";")[0], 16)
            if size == 0:
                while b"\r\n" not in buf:
                    d = c.recv(65536)
                    if not d:
                        return body, buf
                    buf += d
                _, buf = buf.split(b"\r\n", 1)
                return body, buf
            while len(buf) < size + 2:
                d = c.recv(65536)
                if not d:
                    return body, buf
                buf += d
            body += buf[:size]
            buf = buf[size + 2:]
    elif b"content-length" in hdrs:
        n = int(hdrs[b"content-length"])
        while len(buf) < n:
            d = c.recv(65536)
            if not d:
                return body, buf
            buf += d
        body, buf = buf[:n], buf[n:]
    return body, buf


def handle(c):
    global conns
    with lock:
        conns += 1
    my = conns
    buf = b""
    try:
        while True:
            head, buf = read_head(buf, c)
            if head is None:
                return
            lines = head.split(b"\r\n")
            method, target = lines[0].split(b" ")[:2]
            hdrs = {}
            for line in lines[1:]:
                k, _, v = line.partition(b":")
                hdrs[k.strip().lower()] = v.strip()
            body, buf = read_body(c, buf, hdrs)
            payload = (b"UA=" + hdrs.get(b"user-agent", b"<none>") + b"\n"
                       + b"EXPECT=" + hdrs.get(b"expect", b"<none>") + b"\n"
                       + b"BODY=" + body + b"\n"
                       + b"CONNS=" + str(my).encode() + b"\n"
                       + b"TARGET=" + target)
            if target == b"/chunked":
                # 分块响应（带 trailer）：验证代理原样转发分帧
                chunks = b""
                for i in range(0, len(payload), 16):
                    part = payload[i:i + 16]
                    chunks += b"%x;ext=1\r\n%s\r\n" % (len(part), part)
                c.sendall(b"HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n"
                          b"Trailer: X-Sum\r\n\r\n" + chunks + b"0\r\nX-Sum: ok\r\n\r\n")
                continue
            resp = (b"HTTP/1.1 200 OK\r\nContent-Length: " + str(len(payload)).encode()
                    + b"\r\n\r\n")
            if target == b"/interim":
                # 先发 1xx 中间响应，再发真正的响应
                resp = b"HTTP/1.1 100 Continue\r\n\r\n" + resp
            c.sendall(resp if method == b"HEAD" else resp + payload)
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
