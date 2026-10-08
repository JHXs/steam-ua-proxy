// steam-ua-proxy —— 本地 HTTP 代理，只做一件事：把 User-Agent 改成普通浏览器。
//
// 用途：绕过网络侧"上网行为管理"对 Steam 下载的拦截。
// 该设备靠「Steam User-Agent + /depot/*/chunk/* 路径」的组合识别 Steam 游戏更新，
// 改写 UA 后它就不认识了；流量依然是直连国内 Steam CDN，不消耗任何节点/机场流量。
//
// 用法：
//
//	steam-ua-proxy              # 监听 127.0.0.1:8899
//	steam-ua-proxy 8900         # 换端口
//	http_proxy=http://127.0.0.1:8899 steam
//
// 只改写明文 HTTP 的 UA；HTTPS(CONNECT) 原样隧道转发。
package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	listenHost = "127.0.0.1"
	listenPort = "8899"

	connectTimeout = 20 * time.Second
	idleTimeout    = 300 * time.Second
	// 隧道是盲转发，绝不能带上连接超时，否则空闲 20 秒就会被我们这边切断
	// （Steam 的 CM 就是这么掉线的：连上后 20~40 秒被断开）
	tunnelTimeout = time.Duration(0)

	bufSize  = 65536
	verbose  = true
	procName = "steam-ua-proxy"
)

var fakeUA = []byte("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")

var dropHeaders = map[string]bool{
	"user-agent":       true,
	"proxy-connection": true,
	"expect":           true,
}

var httpMethods = map[string]bool{
	"GET": true, "POST": true, "HEAD": true, "PUT": true,
	"DELETE": true, "OPTIONS": true, "PATCH": true, "TRACE": true,
}

var crlfcrlf = []byte("\r\n\r\n")

func logf(format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// setProcessName 设置 comm（Linux PR_SET_NAME）。mihomo 按 /proc/<pid>/exe 匹配进程，
// 这里只是让 ps/top 看起来更清楚。
func setProcessName(name string) {
	if runtime.GOOS != "linux" {
		return
	}
	if len(name) > 15 {
		name = name[:15]
	}
	b := append([]byte(name), 0)
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, 15, uintptr(unsafe.Pointer(&b[0])), 0)
	if errno != 0 {
		fmt.Fprintln(os.Stderr, "设置进程名失败:", errno)
	}
}

func sendAll(c net.Conn, data []byte) error {
	_, err := c.Write(data)
	return err
}

// reader 是带缓冲的 socket 读取器，类比 Python 版的 Reader：
// 支持按分隔符 / 定长 / 读到 EOF，并且每次 recv 都重置空闲超时。
type reader struct {
	c       net.Conn
	buf     []byte
	timeout time.Duration
}

func (r *reader) read(p []byte) (int, error) {
	if r.timeout > 0 {
		_ = r.c.SetReadDeadline(time.Now().Add(r.timeout))
	} else {
		_ = r.c.SetReadDeadline(time.Time{})
	}
	return r.c.Read(p)
}

// readUntil 读到 sep（含 sep）为止；EOF / 出错返回 false。
func (r *reader) readUntil(sep []byte) ([]byte, bool) {
	for {
		if i := bytes.Index(r.buf, sep); i >= 0 {
			n := i + len(sep)
			out := append([]byte(nil), r.buf[:n]...)
			r.buf = append([]byte(nil), r.buf[n:]...)
			return out, true
		}
		tmp := make([]byte, bufSize)
		n, err := r.read(tmp)
		if n > 0 {
			r.buf = append(r.buf, tmp[:n]...)
		}
		if err != nil {
			return nil, false
		}
	}
}

// readExact 尽量读满 n 字节；出错 / EOF 时返回已读到的部分。
func (r *reader) readExact(n int) []byte {
	for len(r.buf) < n {
		want := bufSize
		if rem := n - len(r.buf); rem < want {
			want = rem
		}
		tmp := make([]byte, want)
		got, err := r.read(tmp)
		if got > 0 {
			r.buf = append(r.buf, tmp[:got]...)
		}
		if err != nil {
			break
		}
	}
	if n > len(r.buf) {
		n = len(r.buf)
	}
	out := append([]byte(nil), r.buf[:n]...)
	r.buf = append([]byte(nil), r.buf[n:]...)
	return out
}

func (r *reader) readToEOF() []byte {
	out := append([]byte(nil), r.buf...)
	r.buf = nil
	for {
		tmp := make([]byte, bufSize)
		n, err := r.read(tmp)
		if n > 0 {
			out = append(out, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return out
}

// rewriteRequest 把请求行里的绝对 URL 换成 path，并替换 User-Agent。
func rewriteRequest(head, path []byte) []byte {
	lines := bytes.Split(head, []byte("\r\n"))
	reqLine := bytes.SplitN(lines[0], []byte(" "), 3)
	out := [][]byte{bytes.Join([][]byte{reqLine[0], path, []byte("HTTP/1.1")}, []byte(" "))}
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		name := bytes.ToLower(bytes.TrimSpace(bytes.SplitN(line, []byte(":"), 2)[0]))
		if dropHeaders[string(name)] {
			continue
		}
		out = append(out, line)
	}
	out = append(out, append([]byte("User-Agent: "), fakeUA...))
	return append(bytes.Join(out, []byte("\r\n")), crlfcrlf...)
}

// rewriteHeaders 只换 User-Agent，不动请求行（隧道里的请求行已经是相对路径）。
func rewriteHeaders(head []byte) []byte {
	lines := bytes.Split(head, []byte("\r\n"))
	out := [][]byte{lines[0]}
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		name := bytes.ToLower(bytes.TrimSpace(bytes.SplitN(line, []byte(":"), 2)[0]))
		if dropHeaders[string(name)] {
			continue
		}
		out = append(out, line)
	}
	out = append(out, append([]byte("User-Agent: "), fakeUA...))
	return append(bytes.Join(out, []byte("\r\n")), crlfcrlf...)
}

// headerValue 从请求/响应头里取某个头的值（没有就返回 nil）。
func headerValue(head []byte, name string) []byte {
	if len(head) < 4 {
		return nil
	}
	for _, line := range bytes.Split(head[:len(head)-4], []byte("\r\n"))[1:] {
		n, v, _ := bytes.Cut(line, []byte(":"))
		if string(bytes.ToLower(bytes.TrimSpace(n))) == name {
			return bytes.TrimSpace(v)
		}
	}
	return nil
}

func firstToken(head []byte) string {
	tok, _, _ := bytes.Cut(head, []byte(" "))
	return string(bytes.ToUpper(tok))
}

func closeWrite(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
}

// tunnel 双向盲转发中的单向：src → dst，结束后半关闭两端。
func tunnel(src, dst net.Conn, initial []byte) {
	if len(initial) > 0 {
		if err := sendAll(dst, initial); err != nil {
			closeWrite(src)
			closeWrite(dst)
			return
		}
	}
	buf := make([]byte, bufSize)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if werr := sendAll(dst, buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	closeWrite(src)
	closeWrite(dst)
}

// serveTunneledHTTP CONNECT 到 80 端口：隧道里的每个明文 HTTP 请求也要换掉 UA。
//
// mihomo 的 http 出站就是这么走的 —— 先 CONNECT 建隧道，再把明文请求写进隧道。
// 如果这里只做盲转发，UA 就改不掉，拦截照样命中。
func serveTunneledHTTP(client, upstream net.Conn, initial []byte) {
	r := &reader{c: client, buf: initial, timeout: tunnelTimeout}
	ur := &reader{c: upstream, timeout: tunnelTimeout}
	for {
		head, ok := r.readUntil(crlfcrlf)
		if !ok {
			return
		}
		method := firstToken(head)
		if !httpMethods[method] {
			// 不是明文 HTTP（比如 TLS），退回盲转发
			leftover := r.buf
			r.buf = nil
			if err := sendAll(upstream, append(head, leftover...)); err != nil {
				return
			}
			go tunnel(client, upstream, nil)
			tunnel(upstream, client, nil)
			return
		}
		if err := sendAll(upstream, rewriteHeaders(head)); err != nil {
			return
		}
		if !relayRequestBody(r, upstream, head) {
			return
		}
		if !forwardResponse(ur, client, method) {
			return
		}
	}
}

// relayRequestBody 把请求体也转发给上游。
//
// 只转请求头的话，带 body 的请求（POST 等）会把剩下的 body 字节当成下一个
// 请求头来解析，整条连接直接破帧 —— 表现就是经代理的 POST 全部失败。
func relayRequestBody(r *reader, upstream net.Conn, head []byte) bool {
	if te := headerValue(head, "transfer-encoding"); te != nil &&
		bytes.Contains(bytes.ToLower(te), []byte("chunked")) {
		for {
			line, ok := r.readUntil([]byte("\r\n"))
			if !ok {
				return false
			}
			if err := sendAll(upstream, line); err != nil {
				return false
			}
			size, err := strconv.ParseInt(string(bytes.SplitN(bytes.TrimSpace(line), []byte(";"), 2)[0]), 16, 64)
			if err != nil {
				return false
			}
			if size == 0 {
				for {
					trailer, ok := r.readUntil([]byte("\r\n"))
					if !ok {
						return false
					}
					if err := sendAll(upstream, trailer); err != nil {
						return false
					}
					if bytes.Equal(trailer, []byte("\r\n")) {
						return true
					}
				}
			}
			data := r.readExact(int(size))
			if len(data) != int(size) {
				return false
			}
			if err := sendAll(upstream, data); err != nil {
				return false
			}
			if err := sendAll(upstream, r.readExact(2)); err != nil { // 每个 chunk 后面的 CRLF
				return false
			}
		}
	}

	if length := headerValue(head, "content-length"); length != nil {
		remaining, err := strconv.Atoi(string(length))
		if err != nil {
			return false
		}
		for remaining > 0 {
			want := bufSize
			if remaining < want {
				want = remaining
			}
			piece := r.readExact(want)
			if len(piece) == 0 {
				return false
			}
			if err := sendAll(upstream, piece); err != nil {
				return false
			}
			remaining -= len(piece)
		}
	}
	return true
}

// forwardResponse 按 HTTP 分帧把响应完整转发回去；返回是否还能继续复用连接。
func forwardResponse(src *reader, dst net.Conn, method string) bool {
	var head []byte
	for {
		var ok bool
		head, ok = src.readUntil(crlfcrlf)
		if !ok {
			return false
		}
		if err := sendAll(dst, head); err != nil {
			return false
		}
		statusLine := bytes.SplitN(head, []byte("\r\n"), 2)[0]
		fields := bytes.Fields(statusLine)
		if len(fields) < 2 {
			return false
		}
		status, err := strconv.Atoi(string(fields[1]))
		if err != nil {
			return false
		}
		if status >= 200 { // 1xx 是中间响应，转完继续读真正的响应
			break
		}
	}

	headers := map[string]string{}
	for _, line := range bytes.Split(head[:len(head)-4], []byte("\r\n"))[1:] {
		name, value, _ := bytes.Cut(line, []byte(":"))
		headers[string(bytes.ToLower(bytes.TrimSpace(name)))] = string(bytes.ToLower(bytes.TrimSpace(value)))
	}
	status, _ := strconv.Atoi(string(bytes.Fields(bytes.SplitN(head, []byte("\r\n"), 2)[0])[1]))

	if method == "HEAD" || status == 204 || status == 304 {
		return true
	}
	if strings.Contains(headers["transfer-encoding"], "chunked") {
		for {
			line, ok := src.readUntil([]byte("\r\n"))
			if !ok {
				return false
			}
			if err := sendAll(dst, line); err != nil {
				return false
			}
			size, err := strconv.ParseInt(string(bytes.SplitN(bytes.TrimSpace(line), []byte(";"), 2)[0]), 16, 64)
			if err != nil {
				return false
			}
			if size == 0 {
				for {
					trailer, ok := src.readUntil([]byte("\r\n"))
					if !ok {
						return false
					}
					if err := sendAll(dst, trailer); err != nil {
						return false
					}
					if bytes.Equal(trailer, []byte("\r\n")) {
						return true
					}
				}
			}
			data := src.readExact(int(size))
			if len(data) != int(size) {
				return false
			}
			if err := sendAll(dst, data); err != nil {
				return false
			}
			if err := sendAll(dst, src.readExact(2)); err != nil {
				return false
			}
		}
	}
	if cl, ok := headers["content-length"]; ok {
		remaining, err := strconv.Atoi(cl)
		if err != nil {
			return false
		}
		for remaining > 0 {
			want := bufSize
			if remaining < want {
				want = remaining
			}
			piece := src.readExact(want)
			if len(piece) == 0 {
				return false
			}
			if err := sendAll(dst, piece); err != nil {
				return false
			}
			remaining -= len(piece)
		}
		return true
	}
	// 读到连接关闭为止
	_ = sendAll(dst, src.readToEOF())
	return false
}

func serveConnection(client net.Conn) {
	defer client.Close()
	r := &reader{c: client, timeout: idleTimeout}

	var upstream net.Conn
	var upstreamKey string
	var upReader *reader

	defer func() {
		if upstream != nil {
			_ = upstream.Close()
		}
	}()

	for {
		head, ok := r.readUntil(crlfcrlf)
		if !ok {
			return
		}
		line := bytes.SplitN(head, []byte("\r\n"), 2)[0]
		parts := bytes.Fields(line)
		if len(parts) < 3 {
			return
		}
		method := string(bytes.ToUpper(parts[0]))
		target := string(parts[1])

		if method == "CONNECT" {
			host, portStr, found := strings.Cut(target, ":")
			if !found {
				portStr = "443"
			}
			port, err := strconv.Atoi(portStr)
			if err != nil {
				return
			}
			server, err := net.DialTimeout("tcp", net.JoinHostPort(host, portStr), connectTimeout)
			if err != nil {
				logf("连接结束: dial %s:%s: %v", host, portStr, err)
				return
			}
			if err := sendAll(client, []byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
				_ = server.Close()
				return
			}
			// 隧道是盲转发，两端都清掉超时
			_ = server.SetDeadline(time.Time{})
			_ = client.SetDeadline(time.Time{})
			leftover := r.buf
			r.buf = nil
			if port == 80 {
				// 明文 HTTP 隧道：里面也要换 UA（mihomo 把本代理当节点用时就是这种）
				serveTunneledHTTP(client, server, leftover)
				_ = server.Close()
				return
			}
			go tunnel(client, server, leftover)
			tunnel(server, client, nil)
			_ = server.Close()
			return
		}

		// 只接受绝对形式的 http:// URL（普通 HTTP 代理请求）
		rest, ok := strings.CutPrefix(target, "http://")
		if !ok {
			return
		}
		hostport := rest
		path := "/"
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			hostport, path = rest[:i], rest[i:]
		}
		if hostport == "" {
			return
		}
		host, port := hostport, "80"
		if i := strings.IndexByte(hostport, ':'); i >= 0 {
			host, port = hostport[:i], hostport[i+1:]
		}
		key := host + ":" + port

		if upstream == nil || key != upstreamKey {
			if upstream != nil {
				_ = upstream.Close()
			}
			c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), connectTimeout)
			if err != nil {
				logf("连接结束: dial %s: %v", key, err)
				upstream, upstreamKey, upReader = nil, "", nil
				return
			}
			upstream = c
			upstreamKey = key
			upReader = &reader{c: c, timeout: idleTimeout}
			logf("新连接 %s", key)
		}

		if err := sendAll(upstream, rewriteRequest(head, []byte(path))); err != nil {
			break
		}
		if !relayRequestBody(r, upstream, head) {
			break
		}
		if !forwardResponse(upReader, client, method) {
			break
		}
	}

	if upstream != nil {
		_ = upstream.Close()
		upstream, upstreamKey, upReader = nil, "", nil
	}
}

func main() {
	setProcessName(procName)
	port := listenPort
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(listenHost, port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "监听失败:", err)
		os.Exit(1)
	}
	fmt.Printf("steam-ua-proxy 监听 %s:%s\n", listenHost, port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go serveConnection(conn)
	}
}
