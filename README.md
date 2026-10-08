# steam-ua-proxy

本地 HTTP 代理，只做一件事：把明文 HTTP 的 `User-Agent` 改成普通浏览器 UA，
绕过网络侧「Steam UA + `/depot/*/chunk/*`」的上网行为管理拦截。HTTPS(CONNECT) 只做隧道转发。
监听 `127.0.0.1:8899`（可传参换端口）。背景与排障见 `~/Documents/Agent/Steam下载修复记录.md`。

## 目录

| 目录 | 内容 |
|---|---|
| `go/` | Go 实现（当前推荐）：`main.go`、`go.mod`、`Makefile` |
| `python/` | Python 原版：`steam-ua-proxy.py`、`pyproject.toml`、`uv.lock`、`.venv` |
| `scripts/` | 两版共用的行为对等测试 |

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
