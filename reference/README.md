# reference/ —— 已冻结的 Python 参考实现

这里是 `steam-ua-proxy` 最初的 Python 实现（PyInstaller onefile 打包，约 11.4 MB）。
**已冻结，不再维护，也不建议用于新部署。**

保留它的唯一目的：`make -C go test` 会在 `scripts/run-tests.sh` 里把它和 Go 版
一起跑一遍同一套行为对等测试（`scripts/ua_test.py`），作为 Go 版改写行为的基准。
测试只验证协议行为一致，不代表两者性能相当。

## 为什么改用 Go 版

| | Go 版（`go/`） | Python 版（本目录） |
|---|---|---|
| 体积 | ~2.2 MB 静态 ELF，无运行时依赖 | ~11.4 MB，含解释器 |
| 隧道转发 | `io.Copy` → Linux `splice(2)` 零拷贝 | 纯用户态收发，无零拷贝 |
| 跨平台发布 | `CGO_ENABLED=0` 交叉编译单文件 | 需在目标架构上打包 |

两者协议行为保持一致（绝对 URL → 相对路径改写、`Drop` 掉 `User-Agent` /
`Proxy-Connection` / `Expect`、请求体按 `Content-Length` 与 `chunked` 分帧转发、
跳过 `1xx`、`HEAD`/`204`/`304` 不读 body、keep-alive 复用上游连接、CONNECT 盲转发、
CONNECT 到 80 端口时在隧道内逐请求改写 UA、隧道不设超时），并共享同一个 bug 修复：
盲转发时只对目标端 `shutdown(SHUT_WR)`，不再连带掐掉反方向的响应。

## 用法

仅用于复现基准（Python 版仍是位置参数传端口；Go 版已改成 `-p/--port`）：

```bash
python3 steam-ua-proxy.py 8899        # 直接运行
```

重新打包：

```bash
cd reference
uv run pyinstaller --clean --noconfirm --onefile --name steam-ua-proxy steam-ua-proxy.py
```

`pyproject.toml` 里设了 `[tool.uv] package = false`：本目录不是 Python 包，
只用来固定 `pyinstaller` 依赖，不需要 `src/steam_ua_proxy/`。
打包需在目标 Linux 架构上进行；若当前 Python 版本不受 PyInstaller 支持，
使用其支持的 Python 版本构建。
