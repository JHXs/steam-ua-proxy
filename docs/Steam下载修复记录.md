# Steam 下载修复记录

> 环境：EndeavourOS / Linux x86_64；代理监听 `127.0.0.1:8899`。记录时间：2026-10-07
> 本仓库提供两个等价实现（`go/` 与 `python/`），本机当前部署的是 PyInstaller 打包的 Python 版。
> 代码与构建方式见根目录 [README](../README.md)。

## 问题与结论

Steam 下载国内 CDN 时被网络侧上网行为管理拦截，返回 `1.1.1.3/disable/disable.htm`。实测拦截条件是 **Steam User-Agent + `/depot/.../chunk/...` 路径**；普通浏览器 UA 请求相同 URL 可通过。

本地代理只改写明文 HTTP 的 `User-Agent`，HTTPS `CONNECT` 隧道原样转发。Steam 下载流量仍直连国内 CDN，不消耗机场流量。实测下载约 **94 Mbps**，达到百兆网卡上限。

## 工作方式

```text
Steam ──HTTP──> 127.0.0.1:8899 ──改写 UA──> 国内 Steam CDN
                   ↑
        Mihomo SteamUA HTTP 节点
```

Sparkle 开启 TUN 时，Steam CDN 域名规则交给 `SteamUA` 节点；代理进程自己的外连必须 `DIRECT`，否则可能被 TUN 再送回 `SteamUA` 形成循环。

## 文件与运行服务

仓库内容：

| 路径 | 用途 |
|---|---|
| `go/` | Go 实现（静态 ELF，约 2.2 MB），`make -C go build` |
| `python/` | Python 原版 + PyInstaller onefile 打包配置（约 11.4 MB） |
| `scripts/` | 两版共用的行为对等测试（`make -C go test`） |
| `docs/Steam下载修复记录.md` | 本文档 |

本机部署：

| 路径 | 用途 |
|---|---|
| `~/.local/bin/steam-ua-proxy` | 常驻服务实际运行的可执行文件 |
| `~/.local/bin/steam-ua-proxy.py` | 代理 Python 源码（备用/对比用） |
| `~/.config/systemd/user/steam-ua-proxy.service` | 常驻服务，开机启动、异常自动重启 |
| `~/.config/sparkle/override/1a1d0c0ffee.yaml` | 持久化 Sparkle/Mihomo 节点和规则覆写 |
| `~/.config/sparkle/work/config.yaml` | Sparkle 当前生成的运行配置 |
| `~/.local/bin/steam-download` | 非 TUN 下载模式的一键启动脚本 |

服务命令：

```bash
systemctl --user status steam-ua-proxy.service
systemctl --user restart steam-ua-proxy.service
journalctl --user -u steam-ua-proxy.service -n 50 --no-pager
```

服务配置中的启动行为：

```ini
ExecStart=%h/.local/bin/steam-ua-proxy
Restart=always
RestartSec=2
```

## 把 Python 脚本打包成独立程序

Mihomo 在 Linux 上按 `/proc/<PID>/exe` 识别进程，不看 Python 的 `comm` 名称，也不看脚本文件名。直接运行脚本时，即使调用了 `prctl` 设置进程名，Mihomo 仍会把它识别为 Python 可执行文件（本机是 `python3.14`）。所以要得到一个能被 `PROCESS-NAME,steam-ua-proxy` 命中的进程，就得把它编译成独立可执行文件。两条路：

**Go 版（推荐，体积最小）**—— `go/steam-ua-proxy` 是静态链接的单个 ELF，约 2.2 MB：

```bash
make -C go build      # CGO_ENABLED=0 + -trimpath -ldflags "-s -w"
make -C go install    # 装到 ~/.local/bin 并重启用户服务
```

**PyInstaller 版**——把解释器和脚本一起打包，约 11.4 MB：

```bash
cd python
uv run pyinstaller --clean --noconfirm --onefile --name steam-ua-proxy steam-ua-proxy.py
install -m 755 dist/steam-ua-proxy ~/.local/bin/steam-ua-proxy
```

两版行为一致（见 `scripts/` 里的对等测试）。打包需在目标 Linux 架构上进行；若当前 Python 版本不受 PyInstaller 支持，使用其支持的 Python 版本构建。验证可执行文件身份：

```bash
~/.local/bin/steam-ua-proxy 8898 >/tmp/steam-ua-proxy-test.log 2>&1 &
pid=$!
sleep 1
readlink /proc/$pid/exe    # 应指向 ~/.local/bin/steam-ua-proxy
kill "$pid"
```

## Mihomo / Sparkle 配置

在持久化覆写中保留 `SteamUA` HTTP 节点，并将代理专属直连规则放在 Steam CDN 规则之前：

```yaml
proxies+:
  - name: SteamUA
    type: http
    server: 127.0.0.1
    port: 8899
    udp: false

+rules:
  - PROCESS-NAME,steam-ua-proxy,DIRECT
  - DOMAIN-SUFFIX,clngaa.com,SteamUA
  - DOMAIN-SUFFIX,eccdnx.com,SteamUA
  - DOMAIN-SUFFIX,pphimalayanrt.com,SteamUA
  - DOMAIN-SUFFIX,steamcontent.com,SteamUA
  - DOMAIN-SUFFIX,steampipe.akamaized.net,SteamUA
  - DOMAIN-SUFFIX,steamcdn-a.akamaihd.net,SteamUA
```

`PROCESS-NAME,steam-ua-proxy,DIRECT` 只让代理进程本身直连，避免把其他 Python 程序也设为直连。直接跑 `.py` 时进程名是 `python3.14`，这条规则反而会扩大成「所有 Python 程序直连」——所以务必用编译后的可执行文件。当前配置启用了 `find-process-mode: always`。规则或覆写有改动后，在 Sparkle 重载 Mihomo 配置。

## 使用与验证

- **TUN 模式**：保持 `steam-ua-proxy.service` 运行，Steam 正常启动并下载；Steam CDN 流量经 `SteamUA`，代理自己的 CDN 外连直连。
- **手动下载模式**：运行 `steam-download`，它会设置 `http_proxy` 并重启 Steam。
- 检查代理监听：`ss -ltnp | grep 8899`
- 检查封禁记录：`grep -c 'disable.htm' ~/.local/share/Steam/logs/content_log.txt`
- Mihomo 日志应显示 Steam CDN 域名命中 `SteamUA`。

> `steam-download` 在 systemd 服务未运行时的备用启动分支仍使用 Python 源码；TUN 使用时应保持 systemd 服务正常运行。

## 已确认的结果与回退

- 相同 chunk URL：经代理返回正常 CDN；完全直连会命中 `1.1.1.3` 封禁页。
- 国内 CDN 下载峰值约 94 Mbps；节点流量消耗为 0。
- 已验证：切换到独立可执行文件和专属 Mihomo 进程规则后，TUN 下可正常使用。

停止并禁用本地服务：

```bash
systemctl --user disable --now steam-ua-proxy.service
```

恢复旧规则时，将 Mihomo 规则改回原配置并在 Sparkle 重载；不要删除 `.py` 源码，除非确定不再需要重新打包。
