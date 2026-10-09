#!/usr/bin/env python3
"""对比测试：Go 版 / Python 版 steam-ua-proxy 的行为是否一致。

用法：ua_test.py <proxy_port> <echo_port> <raw_port> [--slow]
"""
import socket
import sys
import time

PROXY = int(sys.argv[1])
ECHO = int(sys.argv[2])
RAW = int(sys.argv[3])
SLOW = "--slow" in sys.argv

STEAM_UA = "Valve/Steam HTTP Client 1.0 (1446780)"
CHROME = b"Chrome/131"

results = []


def build(method, target, extra=(), body=b""):
    lines = [f"{method} {target} HTTP/1.1", "Host: 127.0.0.1", f"User-Agent: {STEAM_UA}"]
    lines.extend(extra)
    return ("\r\n".join(lines) + "\r\n\r\n").encode() + body


class Client:
    def __init__(self, port, timeout=20):
        self.s = socket.create_connection(("127.0.0.1", port), timeout=timeout)
        self.buf = b""

    def send(self, data):
        self.s.sendall(data)

    def _fill(self):
        d = self.s.recv(65536)
        if not d:
            raise EOFError("上游关闭")
        self.buf += d

    def read_head(self):
        while b"\r\n\r\n" not in self.buf:
            self._fill()
        head, self.buf = self.buf.split(b"\r\n\r\n", 1)
        return head

    def read_response(self):
        head = self.read_head()
        status = int(head.split(b"\r\n")[0].split(b" ")[1])
        cl, chunked = None, False
        for line in head.split(b"\r\n")[1:]:
            k, _, v = line.partition(b":")
            k, v = k.strip().lower(), v.strip().lower()
            if k == b"content-length":
                cl = int(v)
            if k == b"transfer-encoding" and b"chunked" in v:
                chunked = True
        body = b""
        if chunked:
            while True:
                while b"\r\n" not in self.buf:
                    self._fill()
                line, self.buf = self.buf.split(b"\r\n", 1)
                size = int(line.split(b";")[0], 16)
                if size == 0:
                    while b"\r\n" not in self.buf:
                        self._fill()
                    _, self.buf = self.buf.split(b"\r\n", 1)
                    break
                while len(self.buf) < size + 2:
                    self._fill()
                body += self.buf[:size]
                self.buf = self.buf[size + 2:]
        elif cl is not None:
            while len(self.buf) < cl:
                self._fill()
            body, self.buf = self.buf[:cl], self.buf[cl:]
        return status, body

    def connect(self, hostport):
        self.send(f"CONNECT {hostport} HTTP/1.1\r\n\r\n".encode())
        head = self.read_head()
        return b"200" in head.split(b"\r\n")[0]

    def close(self):
        try:
            self.s.close()
        except OSError:
            pass


def check(name, ok, detail=""):
    results.append(ok)
    print(f"{'PASS' if ok else 'FAIL'}  {name}{'  ' + detail if detail else ''}", flush=True)


def case_get():
    c = Client(PROXY)
    c.send(build("GET", f"http://127.0.0.1:{ECHO}/hello", ["Proxy-Connection: close"]))
    status, body = c.read_response()
    c.close()
    check("GET 绝对 URL：UA 改写 + 请求行变相对路径",
          status == 200 and CHROME in body and b"TARGET=/hello" in body,
          f"status={status} {body[:100]!r}")


def case_post_content_length():
    payload = b"A" * 5120
    c = Client(PROXY)
    c.send(build("POST", f"http://127.0.0.1:{ECHO}/up", [f"Content-Length: {len(payload)}"], payload))
    status, body = c.read_response()
    c.close()
    check("POST Content-Length：body 完整、不破帧",
          status == 200 and b"BODY=" + payload in body and CHROME in body,
          f"status={status} len={len(body)}")


def case_post_chunked():
    c = Client(PROXY)
    body_bytes = b"5\r\nhello\r\n6\r\n world\r\n4\r\n!!!!\r\n0\r\n\r\n"
    c.send(build("POST", f"http://127.0.0.1:{ECHO}/up", ["Transfer-Encoding: chunked"], body_bytes))
    status, body = c.read_response()
    c.close()
    check("POST chunked：body 完整",
          status == 200 and b"BODY=hello world!!!!" in body,
          f"status={status} {body[:110]!r}")


def case_expect_100():
    payload = b"E" * 100
    c = Client(PROXY)
    c.send(build("POST", f"http://127.0.0.1:{ECHO}/up",
                 ["Expect: 100-continue", f"Content-Length: {len(payload)}"], payload))
    status, body = c.read_response()
    c.close()
    check("Expect: 100-continue：不转发 Expect、body 完整",
          status == 200 and b"EXPECT=<none>" in body and b"BODY=" + payload in body,
          f"status={status} {body[:150]!r}")


def case_keepalive():
    c = Client(PROXY)
    conns = []
    for i in range(2):
        c.send(build("GET", f"http://127.0.0.1:{ECHO}/ka{i}"))
        status, body = c.read_response()
        if status != 200:
            c.close()
            check("keep-alive：同连接两次 GET 复用上游", False, f"status={status}")
            return
        conns.append(body.split(b"CONNS=")[1].split(b"\n")[0])
    c.close()
    check("keep-alive：同连接两次 GET 复用上游",
          conns[0] == conns[1] and b"TARGET=/ka1" in body, f"CONNS={conns}")


def case_head():
    c = Client(PROXY)
    c.send(build("HEAD", f"http://127.0.0.1:{ECHO}/head"))
    head = c.read_head()
    status = int(head.split(b"\r\n")[0].split(b" ")[1])
    c.close()
    check("HEAD：只回头、不挂 body", status == 200 and b"Content-Length" in head,
          f"status={status}")


def case_chunked_response():
    c = Client(PROXY)
    c.send(build("GET", f"http://127.0.0.1:{ECHO}/chunked"))
    status, body = c.read_response()
    c.close()
    check("chunked 响应（带 trailer + chunk 扩展）：分帧原样转发",
          status == 200 and CHROME in body and b"TARGET=/chunked" in body,
          f"status={status} {body[:110]!r}")


def case_interim_response():
    c = Client(PROXY)
    c.send(build("GET", f"http://127.0.0.1:{ECHO}/interim"))
    first = c.read_head()  # 代理要先把 1xx 转回来
    status, body = c.read_response()  # 然后才是真正的响应
    c.close()
    line = first.split(b"\r\n")[0]
    check("1xx 中间响应：转发后继续读真正的响应",
          b"100" in line and status == 200 and b"TARGET=/interim" in body,
          f"first={line!r} status={status}")


def case_connect_raw():
    c = Client(PROXY)
    ok = c.connect(f"127.0.0.1:{RAW}")
    c.send(b"ping")
    while b"ECHO:ping" not in c.buf:
        c._fill()
    c.close()
    check("CONNECT 非 80 端口：盲转发隧道", ok and b"ECHO:ping" in c.buf)


def case_connect_80_http():
    c = Client(PROXY)
    ok = c.connect("127.0.0.1:80")
    # 隧道里是相对路径的明文 HTTP（mihomo http 出站就是这样）
    c.send(build("GET", "/tunnel"))
    status, body = c.read_response()
    c.close()
    check("CONNECT 到 80：隧道内明文 HTTP 的 UA 也被改写",
          ok and status == 200 and CHROME in body and b"TARGET=/tunnel" in body,
          f"status={status} {body[:100]!r}")


def case_idle_tunnel():
    c = Client(PROXY, timeout=60)
    c.connect(f"127.0.0.1:{RAW}")
    time.sleep(25)  # 旧实现 20 秒超时会把隧道掐断
    c.send(b"still-alive")
    while b"ECHO:still-alive" not in c.buf:
        c._fill()
    c.close()
    check("CONNECT 隧道空闲 25 秒后仍可用", b"ECHO:still-alive" in c.buf)


for fn in (case_get, case_post_content_length, case_post_chunked, case_expect_100,
           case_keepalive, case_head, case_chunked_response, case_interim_response,
           case_connect_raw, case_connect_80_http):
    try:
        fn()
    except Exception as exc:  # noqa: BLE001
        check(fn.__name__, False, f"异常 {type(exc).__name__}: {exc}")

if SLOW:
    try:
        case_idle_tunnel()
    except Exception as exc:  # noqa: BLE001
        check("case_idle_tunnel", False, f"异常 {type(exc).__name__}: {exc}")

print(f"---- {sum(results)}/{len(results)} 通过 ----", flush=True)
sys.exit(0 if all(results) else 1)
