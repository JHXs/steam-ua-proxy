# steam-ua-proxy

本地 HTTP 代理，只做一件事：把明文 HTTP 的 `User-Agent` 改成普通浏览器 UA，
绕过网络侧「Steam UA + `/depot/*/chunk/*`」的上网行为管理拦截。HTTPS(CONNECT) 只做隧道转发。
监听 `127.0.0.1:8899`（可传参换端口）。

完整的背景、实测数据、Mihomo/Sparkle 配置与回退方法见
[Steam 下载修复记录](docs/Steam下载修复记录.md)。

## 目录

| 目录 | 内容 |
|---|---|
| `go/` | Go 实现（推荐）：`main.go`、`go.mod`、`Makefile` |
| `python/` | Python 原版：`steam-ua-proxy.py`、`pyproject.toml`、`uv.lock`、`.venv` |
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
VER=v0.1.1
BASE=https://github.com/JHXs/steam-ua-proxy/releases/download/$VER
curl -fsSLO $BASE/steam-ua-proxy-$VER-linux-amd64
curl -fsSLO $BASE/steam-ua-proxy-$VER-linux-amd64.sha256
sha256sum -c steam-ua-proxy-$VER-linux-amd64.sha256
install -m 755 steam-ua-proxy-$VER-linux-amd64 ~/.local/bin/steam-ua-proxy
```

Windows 直接运行 `steam-ua-proxy-<版本>-windows-amd64.exe`。TUN 下放行代理自身外连（进程名就是可执行文件名）：

```yaml
- PROCESS-NAME,steam-ua-proxy,DIRECT        # Linux
- PROCESS-NAME,steam-ua-proxy.exe,DIRECT    # Windows
```

发布由 [GitHub Actions](.github/workflows/release.yml) 完成，打 tag 即自动构建并上传：

```bash
git tag -a v0.1.2 -m "v0.1.2" && git push origin v0.1.2
```

二进制不入库（体积大且平台相关），也无需本地手动上传；跨平台构建靠 Go 的交叉编译（`CGO_ENABLED=0`）。

两版协议行为保持一致：绝对 URL → 相对路径改写、`Drop` 掉 `User-Agent`/`Proxy-Connection`/`Expect`、
请求体按 `Content-Length` 与 `chunked` 分帧转发、跳过 `1xx`、`HEAD`/`204`/`304` 不读 body、
keep-alive 复用上游连接、CONNECT 盲转发、CONNECT 到 80 端口时在隧道内逐请求改写 UA。
隧道不设任何超时（设了会在空闲时被自己掐断）。

## 构建

```bash
# Go 版（静态、去符号表，约 2.2 MB）
make -C go build          # → go/steam-ua-proxy
make -C go install        # 装到 ~/.local/bin 并重启用户服务

# Python 版（PyInstaller onefile，约 11.4 MB）
cd python
uv run pyinstaller --clean --noconfirm --onefile --name steam-ua-proxy steam-ua-proxy.py
```

> `python/pyproject.toml` 里设了 `[tool.uv] package = false`：本目录不是 Python 包，
> 只用来固定 `pyinstaller` 依赖，不需要 `src/steam_ua_proxy/`。

Go 工具链：若 `go` 不在 PATH，`go/Makefile` 会回退到 `~/apps/go/bin/go`。

## 测试

```bash
make -C go test
```

在独立的 user+net namespace 里同时跑 Go 版（8898）和 Python 版（8897），
这样普通用户也能监听 80 端口，覆盖「CONNECT 到 80 端口后在隧道内改写 UA」这条关键路径。
需要 `unshare -rn` 可用；不支持时该用例会失败，其余用例仍有效。
