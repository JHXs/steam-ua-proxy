// steam-ua-proxy —— 本地 HTTP 代理，只做一件事：把 User-Agent 改成普通浏览器。
//
// 用途：绕过网络侧"上网行为管理"对 Steam 下载的拦截。
// 该设备靠「Steam User-Agent + /depot/*/chunk/* 路径」的组合识别 Steam 游戏更新，
// 改写 UA 后它就不认识了；流量依然是直连国内 Steam CDN，不消耗任何节点/机场流量。
//
// 用法：
//
//	steam-ua-proxy              # 监听 127.0.0.1:8899
//	steam-ua-proxy -p 8900      # 换端口
//	steam-ua-proxy -v           # 打印版本
//	http_proxy=http://127.0.0.1:8899 steam
//
// 只改写明文 HTTP 的 UA；HTTPS(CONNECT) 原样隧道转发。
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// 版本信息：构建时由链接器注入（make build 与 release workflow 都传 -X）；
// 源码里只是默认值，裸 go build 就是 "dev"。
var (
	version = "dev"
	commit  = ""
)

const usageText = `steam-ua-proxy —— 本地 HTTP 代理，只把明文 HTTP 的 User-Agent 换成普通浏览器 UA

用法：steam-ua-proxy [-p 端口]   # 监听 127.0.0.1:端口，默认 8899
      steam-ua-proxy -v         # 版本信息
      steam-ua-proxy -h         # 本帮助

示例：http_proxy=http://127.0.0.1:8899 steam
`

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

var (
	crlf     = []byte("\r\n")
	crlfcrlf = []byte("\r\n\r\n")
)

func logf(format string, args ...any) {
	if !verbose {
		return
	}
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// send 写过去；写失败就说明这条连接不能用了（所有调用点都只关心成败）。
func send(w io.Writer, data []byte) bool {
	_, err := w.Write(data)
	return err == nil
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

// take 取走并清空缓冲区里已读到的剩余字节（改走盲转发时要把它先吐出去）。
func (r *reader) take() []byte {
	out := r.buf
	r.buf = nil
	return out
}

// rewrite 重写请求头：请求行换成 reqLine（nil = 保留原样），并替换 User-Agent。
func rewrite(head, reqLine []byte) []byte {
	lines := bytes.Split(head, crlf)
	out := [][]byte{lines[0]}
	if reqLine != nil {
		out[0] = reqLine
	}
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		if name, _ := headerKV(line); dropHeaders[name] {
			continue
		}
		out = append(out, line)
	}
	out = append(out, append([]byte("User-Agent: "), fakeUA...))
	return append(bytes.Join(out, crlf), crlfcrlf...)
}

// rewriteRequest 普通代理：请求行是绝对 URL，换成 path。
func rewriteRequest(head, path []byte) []byte {
	method, _, _ := bytes.Cut(head, []byte(" "))
	return rewrite(head, bytes.Join([][]byte{method, path, []byte("HTTP/1.1")}, []byte(" ")))
}

// rewriteHeaders 隧道里的请求行已经是相对路径，只换 UA。
func rewriteHeaders(head []byte) []byte { return rewrite(head, nil) }

// headerKV 把一行头拆成字段名（小写）和值。
func headerKV(line []byte) (name string, value []byte) {
	n, v, _ := bytes.Cut(line, []byte(":"))
	return string(bytes.ToLower(bytes.TrimSpace(n))), bytes.TrimSpace(v)
}

// headerValue 从请求/响应头里取某个头的值（没有就返回 nil）。
func headerValue(head []byte, name string) []byte {
	if len(head) < len(crlfcrlf) {
		return nil
	}
	for _, line := range bytes.Split(head[:len(head)-len(crlfcrlf)], crlf)[1:] {
		if n, v := headerKV(line); n == name {
			return v
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

// tunnel 双向盲转发中的单向：src → dst。
//
// 只对 dst 做半关闭：src 已经读到 EOF，替它关写方向是越界，而且会连带掐掉
// 反方向正在写回来的响应（客户端发完请求 half-close 后就被截断）。
func tunnel(src, dst net.Conn, initial []byte) {
	if len(initial) > 0 && !send(dst, initial) {
		closeWrite(dst)
		return
	}
	// TCP↔TCP 走 splice(2) 零拷贝，比手写 Read/Write 循环 CPU 低得多
	_, _ = io.Copy(dst, src)
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
			if !send(upstream, append(head, r.take()...)) {
				return
			}
			go tunnel(client, upstream, nil)
			tunnel(upstream, client, nil)
			return
		}
		if !send(upstream, rewriteHeaders(head)) || !relayRequestBody(r, upstream, head) {
			return
		}
		if !forwardResponse(ur, client, method) {
			return
		}
	}
}

// isChunked 判断 head 是不是 chunked 编码。
func isChunked(head []byte) bool {
	te := headerValue(head, "transfer-encoding")
	return bytes.Contains(bytes.ToLower(te), []byte("chunked"))
}

// chunkSize 解析 chunk 长度行（如 "1a3f\r\n"，忽略 ";ext" 段）。
func chunkSize(line []byte) (int, bool) {
	n, err := strconv.ParseInt(string(bytes.SplitN(bytes.TrimSpace(line), []byte(";"), 2)[0]), 16, 64)
	return int(n), err == nil && n >= 0
}

// copyN 转发定长 body 的 n 字节。
func copyN(r *reader, w io.Writer, n int) bool {
	for n > 0 {
		piece := r.readExact(min(bufSize, n))
		if len(piece) == 0 || !send(w, piece) {
			return false
		}
		n -= len(piece)
	}
	return true
}

// copyChunked 转发 chunked body。
func copyChunked(r *reader, w io.Writer) bool {
	for {
		line, ok := r.readUntil(crlf)
		if !ok || !send(w, line) {
			return false
		}
		size, ok := chunkSize(line)
		if !ok {
			return false
		}
		if size == 0 {
			return copyTrailer(r, w)
		}
		data := r.readExact(size + 2) // chunk 数据 + 后面的 CRLF
		if len(data) != size+2 || !send(w, data) {
			return false
		}
	}
}

// copyTrailer 转发 chunked 末尾的 trailer，直到空行。
func copyTrailer(r *reader, w io.Writer) bool {
	for {
		line, ok := r.readUntil(crlf)
		if !ok || !send(w, line) {
			return false
		}
		if bytes.Equal(line, crlf) {
			return true
		}
	}
}

// relayRequestBody 把请求体也转发给上游。
//
// 只转请求头的话，带 body 的请求（POST 等）会把剩下的 body 字节当成下一个
// 请求头来解析，整条连接直接破帧 —— 表现就是经代理的 POST 全部失败。
func relayRequestBody(r *reader, upstream io.Writer, head []byte) bool {
	if isChunked(head) {
		return copyChunked(r, upstream)
	}
	cl := headerValue(head, "content-length")
	if cl == nil {
		return true // 没有 body
	}
	n, err := strconv.Atoi(string(cl))
	return err == nil && n >= 0 && copyN(r, upstream, n)
}

// forwardResponse 按 HTTP 分帧把响应完整转发回去；返回是否还能继续复用连接。
func forwardResponse(src *reader, dst io.Writer, method string) bool {
	var head []byte
	var status int
	for { // 1xx 是中间响应，转完继续读真正的响应
		var ok bool
		head, ok = src.readUntil(crlfcrlf)
		if !ok || !send(dst, head) {
			return false
		}
		fields := bytes.Fields(bytes.SplitN(head, crlf, 2)[0])
		if len(fields) < 2 {
			return false
		}
		if status, _ = strconv.Atoi(string(fields[1])); status >= 200 {
			break
		}
	}

	if method == "HEAD" || status == 204 || status == 304 {
		return true // 这几个响应没有 body
	}
	if isChunked(head) {
		return copyChunked(src, dst)
	}
	if cl := headerValue(head, "content-length"); cl != nil {
		n, err := strconv.Atoi(string(cl))
		return err == nil && n >= 0 && copyN(src, dst, n)
	}
	// 既没长度也不是 chunked：读到连接关闭为止，连接不能复用
	_ = send(dst, src.readToEOF())
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
		line := bytes.SplitN(head, crlf, 2)[0]
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
			if !send(client, []byte("HTTP/1.1 200 Connection Established\r\n\r\n")) {
				_ = server.Close()
				return
			}
			// 隧道是盲转发，两端都清掉超时
			_ = server.SetDeadline(time.Time{})
			_ = client.SetDeadline(time.Time{})
			leftover := r.take()
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

		if !send(upstream, rewriteRequest(head, []byte(path))) ||
			!relayRequestBody(r, upstream, head) ||
			!forwardResponse(upReader, client, method) {
			break
		}
	}

	if upstream != nil {
		_ = upstream.Close()
		upstream, upstreamKey, upReader = nil, "", nil
	}
}

// printVersion 打印版本摘要。
//
// commit 没被 -X 注入时回退到 go 自动嵌入的 build info（在 git 仓库里构建就有），
// 而且 -s -w / -trimpath 都裁不掉它（不在符号表里）。
func printVersion() {
	rev, dirty := "", false
	if bi, ok := debug.ReadBuildInfo(); ok {
		if version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version // go install pkg@v1.2.3 时是那个 tag
		}
		for _, s := range bi.Settings { // vcs.revision / vcs.modified
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if commit == "" { // 只有回退到 build info 时，dirty 才是新信息（-X 的 version 自带 -dirty）
		commit = rev
		if len(commit) > 12 {
			commit = commit[:12]
		}
		if dirty && commit != "" {
			commit += ", dirty"
		}
	}
	if commit != "" {
		version += " (commit " + commit + ")"
	}
	fmt.Printf("steam-ua-proxy %s\nbuilt with %s %s/%s\n",
		version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func main() {
	setProcessName(procName)

	// 参数解析交给标准库 flag（-p 8899 / -p=8899 / --port=8899 都认），
	// 输出与退出码自己接管：io.Discard 掉 flag 自带的打印，位置参数一律报错。
	// 当初就是因为它被默默当成端口，才报出 "lookup tcp/-v: unknown port"。
	port := listenPort
	showVersion := false
	fs := flag.NewFlagSet(procName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&port, "p", listenPort, "监听端口")
	fs.StringVar(&port, "port", listenPort, "监听端口（同 -p）")
	fs.BoolVar(&showVersion, "v", false, "打印版本信息")
	fs.BoolVar(&showVersion, "version", false, "打印版本信息（同 -v）")
	fs.BoolVar(&showVersion, "V", false, "打印版本信息（同 -v）")

	switch err := fs.Parse(os.Args[1:]); {
	case errors.Is(err, flag.ErrHelp): // -h / --help
		fmt.Print(usageText)
		return
	case err != nil:
		fmt.Fprintf(os.Stderr, "%v\n\n%s", err, usageText)
		os.Exit(2)
	}
	switch {
	case showVersion:
		printVersion()
		return
	case fs.NArg() > 0:
		fmt.Fprintf(os.Stderr, "无法识别的参数 %q：端口要用 -p/--port 指定\n\n%s", fs.Arg(0), usageText)
		os.Exit(2)
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
