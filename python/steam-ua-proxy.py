#!/usr/bin/env python3
"""
steam-ua-proxy —— 本地 HTTP 代理，只做一件事：把 User-Agent 改成普通浏览器。

用途：绕过网络侧"上网行为管理"对 Steam 下载的拦截。
该设备靠「Steam User-Agent + /depot/*/chunk/* 路径」的组合识别 Steam 游戏更新，
改写 UA 后它就不认识了；流量依然是直连国内 Steam CDN，不消耗任何节点/机场流量。

用法：
    python3 ~/.local/bin/steam-ua-proxy.py            # 监听 127.0.0.1:8899
    http_proxy=http://127.0.0.1:8899 steam            # 让 Steam 走这个代理

只改写明文 HTTP 的 UA；HTTPS(CONNECT) 原样隧道转发。
"""
import re
import socket
import socketserver
import sys
import threading
import time
import ctypes

LISTEN_HOST = "127.0.0.1"
LISTEN_PORT = 8899
FAKE_UA = b"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
HTTP_METHODS = (
    b"GET", b"POST", b"HEAD", b"PUT", b"DELETE", b"OPTIONS", b"PATCH", b"TRACE"
)

DROP_HEADERS = {b"user-agent", b"proxy-connection", b"expect"}
CONNECT_TIMEOUT = 20
IDLE_TIMEOUT = 300
# 隧道是盲转发，绝不能带上连接超时，否则空闲 20 秒就会被我们这边切断
# （Steam 的 CM 就是这么掉线的：连上后 20~40 秒被断开）
TUNNEL_TIMEOUT = None
BUFSIZE = 65536
VERBOSE = True


def log(*args):
    if VERBOSE:
        print(f"[{time.strftime('%H:%M:%S')}]", *args, flush=True)

def set_process_name(name=b"steam-ua-proxy"):
    try:
        # Linux: PR_SET_NAME = 15，进程名最多 15 字节
        libc = ctypes.CDLL(None, use_errno=True)
        libc.prctl(15, ctypes.c_char_p(name[:15]), 0, 0, 0)
    except Exception as e:
        print("设置进程名失败:", e, file=sys.stderr)

class Reader:
    """带缓冲的 socket 读取器，支持按分隔符 / 定长 / 读到 EOF。"""

    def __init__(self, sock):
        self.sock = sock
        self.buf = b""

    def read_until(self, sep):
        while sep not in self.buf:
            try:
                data = self.sock.recv(BUFSIZE)
            except (socket.timeout, ConnectionResetError, OSError):
                return None
            if not data:
                return None
            self.buf += data
        idx = self.buf.index(sep) + len(sep)
        out, self.buf = self.buf[:idx], self.buf[idx:]
        return out

    def read_exact(self, n):
        while len(self.buf) < n:
            try:
                data = self.sock.recv(min(BUFSIZE, n - len(self.buf)))
            except (socket.timeout, ConnectionResetError, OSError):
                break
            if not data:
                break
            self.buf += data
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def read_to_eof(self):
        out, self.buf = self.buf, b""
        while True:
            try:
                data = self.sock.recv(BUFSIZE)
            except (socket.timeout, ConnectionResetError, OSError):
                break
            if not data:
                break
            out += data
        return out


def rewrite_request(head, path):
    """把请求行里的绝对 URL 换成 path，并替换 User-Agent。"""
    lines = head.split(b"\r\n")
    out = [b" ".join([lines[0].split(b" ", 2)[0], path, b"HTTP/1.1"])]
    for line in lines[1:]:
        if not line:
            continue
        name = line.split(b":", 1)[0].strip().lower()
        if name in DROP_HEADERS:
            continue
        out.append(line)
    out.append(b"User-Agent: " + FAKE_UA)
    return b"\r\n".join(out) + b"\r\n\r\n"


def rewrite_headers(head):
    """只换 User-Agent，不动请求行（隧道里的请求行已经是相对路径）。"""
    lines = head.split(b"\r\n")
    out = [lines[0]]
    for line in lines[1:]:
        if not line:
            continue
        name = line.split(b":", 1)[0].strip().lower()
        if name in DROP_HEADERS:
            continue
        out.append(line)
    out.append(b"User-Agent: " + FAKE_UA)
    return b"\r\n".join(out) + b"\r\n\r\n"


def header_value(head, name):
    """从请求/响应头里取某个头的值（没有就返回 None）。"""
    for line in head[:-4].split(b"\r\n")[1:]:
        n, _, v = line.partition(b":")
        if n.strip().lower() == name:
            return v.strip()
    return None


def serve_tunneled_http(client, server, initial):
    """CONNECT 到 80 端口：隧道里的每个明文 HTTP 请求也要换掉 UA。

    mihomo 的 http 出站就是这么走的 —— 先 CONNECT 建隧道，再把明文请求写进隧道。
    如果这里只做盲转发，UA 就改不掉，拦截照样命中。
    """
    reader = Reader(client)
    reader.buf = initial
    up_reader = Reader(server)
    while True:
        head = reader.read_until(b"\r\n\r\n")
        if head is None:
            return
        if head.split(b" ", 1)[0].upper() not in HTTP_METHODS:
            # 不是明文 HTTP（比如 TLS），退回盲转发
            leftover, reader.buf = reader.buf, b""
            server.sendall(head + leftover)
            threading.Thread(target=tunnel, args=(server, client), daemon=True).start()
            tunnel(client, server)
            return
        server.sendall(rewrite_headers(head))
        if not relay_request_body(reader, server, head):
            return
        if not forward_response(up_reader, client):
            return


def relay_request_body(reader, upstream, head):
    """把请求体也转发给上游。

    以前只转请求头，带 body 的请求（POST 等）会把剩下的 body 字节当成下一个
    请求头来解析，整条连接直接破帧 —— 表现就是经代理的 POST 全部失败。
    """
    te = (header_value(head, b"transfer-encoding") or b"").lower()
    if b"chunked" in te:
        while True:
            line = reader.read_until(b"\r\n")
            if line is None:
                return False
            upstream.sendall(line)
            try:
                size = int(line.strip().split(b";")[0], 16)
            except ValueError:
                return False
            if size == 0:
                while True:
                    trailer = reader.read_until(b"\r\n")
                    if trailer is None:
                        return False
                    upstream.sendall(trailer)
                    if trailer == b"\r\n":
                        return True
            data = reader.read_exact(size)
            if len(data) != size:
                return False
            upstream.sendall(data)
            upstream.sendall(reader.read_exact(2))  # 每个 chunk 后面的 CRLF

    length = header_value(head, b"content-length")
    if length is not None:
        try:
            remaining = int(length)
        except ValueError:
            return False
        while remaining > 0:
            piece = reader.read_exact(min(BUFSIZE, remaining))
            if not piece:
                return False
            upstream.sendall(piece)
            remaining -= len(piece)
    return True


def forward_response(src, dst, method=b"GET"):
    """按 HTTP 分帧把响应完整转发回去；返回是否还能继续复用连接。"""
    while True:
        head = src.read_until(b"\r\n\r\n")
        if head is None:
            return False
        dst.sendall(head)
        try:
            status = int(head.split(b"\r\n", 1)[0].split()[1])
        except (IndexError, ValueError):
            return False
        if status >= 200:  # 1xx 是中间响应，转完继续读真正的响应
            break

    headers = {}
    for line in head[:-4].split(b"\r\n")[1:]:
        name, _, value = line.partition(b":")
        headers[name.strip().lower()] = value.strip().lower()

    if method == b"HEAD" or status in (204, 304):
        return True
    if b"chunked" in headers.get(b"transfer-encoding", b""):
        while True:
            line = src.read_until(b"\r\n")
            if line is None:
                return False
            dst.sendall(line)
            try:
                size = int(line.strip().split(b";")[0], 16)
            except ValueError:
                return False
            if size == 0:
                while True:
                    trailer = src.read_until(b"\r\n")
                    if trailer is None:
                        return False
                    dst.sendall(trailer)
                    if trailer == b"\r\n":
                        return True
            data = src.read_exact(size)
            if len(data) != size:
                return False
            dst.sendall(data)
            crlf = src.read_exact(2)
            dst.sendall(crlf)
    elif b"content-length" in headers:
        remaining = int(headers[b"content-length"])
        while remaining > 0:
            piece = src.read_exact(min(BUFSIZE, remaining))
            if not piece:
                return False
            dst.sendall(piece)
            remaining -= len(piece)
        return True
    else:  # 读到连接关闭为止
        dst.sendall(src.read_to_eof())
        return False


def tunnel(src_sock, dst_sock, initial=b""):
    if initial:
        try:
            dst_sock.sendall(initial)
        except OSError:
            return
    try:
        while True:
            data = src_sock.recv(BUFSIZE)
            if not data:
                break
            dst_sock.sendall(data)
    except OSError:
        pass
    finally:
        for s in (src_sock, dst_sock):
            try:
                s.shutdown(socket.SHUT_WR)
            except OSError:
                pass


def serve_connection(client):
    client.settimeout(IDLE_TIMEOUT)
    reader = Reader(client)
    upstream = None
    upstream_key = None
    up_reader = None
    try:
        while True:
            head = reader.read_until(b"\r\n\r\n")
            if head is None:
                break
            parts = head.split(b"\r\n", 1)[0].split()
            if len(parts) < 3:
                break
            method, target = parts[0].upper(), parts[1]

            if method == b"CONNECT":
                host, _, port = target.partition(b":")
                port = int(port or b"443")
                server = socket.create_connection(
                    (host.decode(), port), timeout=CONNECT_TIMEOUT
                )
                client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
                server.settimeout(TUNNEL_TIMEOUT)
                client.settimeout(TUNNEL_TIMEOUT)
                leftover, reader.buf = reader.buf, b""
                if port == 80:
                    # 明文 HTTP 隧道：里面也要换 UA（mihomo 把本代理当节点用时就是这种）
                    serve_tunneled_http(client, server, leftover)
                    return
                threading.Thread(
                    target=tunnel, args=(client, server, leftover), daemon=True
                ).start()
                tunnel(server, client)
                return

            match = re.match(rb"http://([^/]+)(/.*)?$", target)
            if not match:
                break
            hostport, path = match.group(1), match.group(2) or b"/"
            if b":" in hostport:
                host, _, port = hostport.partition(b":")
            else:
                host, port = hostport, b"80"
            key = (host, port)

            if upstream is None or key != upstream_key:
                if upstream is not None:
                    upstream.close()
                upstream = socket.create_connection(
                    (host.decode(), int(port)), timeout=CONNECT_TIMEOUT
                )
                upstream.settimeout(IDLE_TIMEOUT)
                upstream_key = key
                up_reader = Reader(upstream)
                log("新连接", "%s:%s" % (host.decode(errors='replace'), port.decode()))

            upstream.sendall(rewrite_request(head, path))
            if not relay_request_body(reader, upstream, head):
                upstream.close()
                upstream = None
                upstream_key = None
                break
            if not forward_response(up_reader, client, method):
                upstream.close()
                upstream = None
                upstream_key = None
    except (socket.timeout, ConnectionResetError, OSError) as exc:
        if not isinstance(exc, (ConnectionResetError,)):
            log("连接结束:", type(exc).__name__, exc)
    finally:
        if upstream is not None:
            upstream.close()
        try:
            client.close()
        except OSError:
            pass


class ProxyServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class ProxyHandler(socketserver.BaseRequestHandler):
    def handle(self):
        serve_connection(self.request)


def main():
    set_process_name()
    port = int(sys.argv[1]) if len(sys.argv) > 1 else LISTEN_PORT
    with ProxyServer((LISTEN_HOST, port), ProxyHandler) as server:
        print(f"steam-ua-proxy 监听 {LISTEN_HOST}:{port}", flush=True)
        server.serve_forever()


if __name__ == "__main__":
    main()
