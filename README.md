# steam-ua-proxy

本地 HTTP 代理，只做一件事：把明文 HTTP 的 `User-Agent` 改成普通浏览器 UA，
绕过网络侧「Steam UA + `/depot/*/chunk/*`」的上网行为管理拦截。HTTPS(CONNECT) 只做隧道转发。
监听 `127.0.0.1:8899`（`-p/--port` 换端口），`-v` 打印版本信息。

完整的背景、实测数据、Mihomo/Sparkle 配置与回退方法见
[Steam 下载修复记录](docs/Steam下载修复记录.md)。

## 目录

| 目录 | 内容 |
|---|---|
| `go/` | Go 实现（推荐）：`main.go`、`go.mod`、`Makefile` |
| `reference/` | 已冻结的 Python 参考实现（**不建议使用**，只作为行为对等测试的基准） |
| `scripts/` | 两版共用的行为对等测试 |
| `docs/` | [修复记录](docs/Steam下载修复记录.md) |

## 下载预编译二进制

见 [Releases](https://github.com/JHXs/steam-ua-proxy/releases)：只发布 Go 版（x86_64）。

| Asset | 平台 | 大小 |
|---|---|---|
| `steam-ua-proxy-<版本>-linux-amd64` | Linux x86_64，静态链接、无运行时依赖 | ~2.2 MB |
| `steam-ua-proxy-<版本>-windows-amd64.exe` | Windows x86_64，单文件 | ~2.3 MB |
| `*.sha256` | 对应产物的 SHA256 | |

```bash
VER=v0.2.0
BASE=https://github.com/JHXs/steam-ua-proxy/releases/download/$VER
curl -fsSLO $BASE/steam-ua-proxy-$VER-linux-amd64
curl -fsSLO $BASE/steam-ua-proxy-$VER-linux-amd64.sha256
sha256sum -c steam-ua-proxy-$VER-linux-amd64.sha256
install -m 755 steam-ua-proxy-$VER-linux-amd64 ~/.local/bin/steam-ua-proxy
steam-ua-proxy -v         # 校验装上的就是 $VER：应打印 steam-ua-proxy $VER (commit …)
```

Windows 直接运行 `steam-ua-proxy-<版本>-windows-amd64.exe`。TUN 下放行代理自身外连（进程名就是可执行文件名）：

```yaml
- PROCESS-NAME,steam-ua-proxy,DIRECT        # Linux
- PROCESS-NAME,steam-ua-proxy.exe,DIRECT    # Windows
```

发布由 [GitHub Actions](.github/workflows/release.yml) 完成，打 tag 即自动构建并上传：

```bash
git tag -a v0.2.0 -m "v0.2.0" && git push origin v0.2.0
```

二进制不入库（体积大且平台相关），也无需本地手动上传；跨平台构建靠 Go 的交叉编译（`CGO_ENABLED=0`）。

两版协议行为保持一致（`reference/` 的 Python 版已冻结，仅用于对照）：绝对 URL → 相对路径改写、`Drop` 掉 `User-Agent`/`Proxy-Connection`/`Expect`、
请求体按 `Content-Length` 与 `chunked` 分帧转发（含 chunk 扩展与末尾 trailer）、
`1xx` 中间响应转发后继续读真正的响应、`HEAD`/`204`/`304` 不读 body、
keep-alive 复用上游连接、CONNECT 盲转发、CONNECT 到 80 端口时在隧道内逐请求改写 UA。
隧道不设任何超时（设了会在空闲时被自己掐断）。

## 构建

```bash
# Go 版（静态、去符号表，约 2.2 MB）
make -C go build          # → go/steam-ua-proxy
make -C go version        # 打印构建出来的版本
make -C go install        # 装到 ~/.local/bin 并重启用户服务

# （归档，不推荐）Python 版：PyInstaller onefile，约 11.4 MB
cd reference
uv run pyinstaller --clean --noconfirm --onefile --name steam-ua-proxy steam-ua-proxy.py
```

> `reference/pyproject.toml` 里设了 `[tool.uv] package = false`：该目录不是 Python 包，
> 只用来固定 `pyinstaller` 依赖，不需要 `src/steam_ua_proxy/`。
> Python 版已冻结，只在 `make -C go test` 里充当行为对等基准——它拿不到 Linux 的
> `splice(2)` 零拷贝，体积也是 Go 版的 5 倍，新部署请用 Go 版。

Go 工具链：若 `go` 不在 PATH，`go/Makefile` 会回退到 `~/apps/go/bin/go`。

版本信息（`-v`）由链接器在构建时注入，源码里只是占位默认值：

```bash
go build -ldflags "-s -w -X main.version=v0.1.2 -X main.commit=$(git rev-parse --short HEAD)" .
```

`make -C go build` 会自动填 `git describe --tags --always --dirty`，所以打了 tag 的提交
构建出来就是 `v0.1.2`（工作区不干净则是 `v0.1.2-dirty`）；Release 工作流用同样的 `-X`
参数，并会跑一次 `./产物 -v` 确认注入成功：

```bash
steam-ua-proxy v0.1.2-dirty (commit 769920f)
built with go1.27.1 linux/amd64
```

没注入时（裸 `go build`）回退到 Go 自动嵌入的 build info，从 `vcs.revision` /
`vcs.modified` 拼出 commit——这两段在 `-s -w` 之后依然可读。

## 测试

```bash
make -C go test
```

在独立的 user+net namespace 里同时跑 Go 版（8898）和 `reference/` 下的 Python 参考版（8897），
这样普通用户也能监听 80 端口，覆盖「CONNECT 到 80 端口后在隧道内改写 UA」这条关键路径。
需要 `unshare -rn` 可用；不支持时该用例会失败，其余用例仍有效。
