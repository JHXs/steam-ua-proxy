#!/usr/bin/env bash
#
# 行为对等测试：Go 版 vs reference/ 下的 Python 冻结参考版。
#
# 在独立的 user+net namespace 里跑，这样普通用户也能监听 80 端口，
# 覆盖「CONNECT 到 80 端口后在隧道内改写 UA」这条关键路径。
#
# 用法：scripts/run-tests.sh [go二进制] [python脚本]
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
GO_BIN=${1:-$ROOT/go/steam-ua-proxy}
PY_SCRIPT=${2:-$ROOT/reference/steam-ua-proxy.py}
S=${ROOT}/scripts

# ---- CLI 自检（不需要 namespace）----
# -v 要能打印版本；端口只能经 -p/--port 指定，位置参数必须被拒绝。
# 用 `-p 0` 让内核挑临时端口，避免和本机在跑的 8899 服务撞车。
cli_fail=0
"$GO_BIN" -v 2>/dev/null | grep -q '^steam-ua-proxy ' \
  || { echo 'FAIL CLI  -v 没有打印版本信息'; cli_fail=1; }
for form in '-p 0' '--port 0' '--port=0'; do
  out=$(timeout 2 $GO_BIN $form 2>&1)
  case $out in
    *'监听 127.0.0.1:0'*) echo "PASS CLI  $form 接受端口" ;;
    *) echo "FAIL CLI  $form 没监听：$out"; cli_fail=1 ;;
  esac
done
out=$(timeout 2 "$GO_BIN" 8899 2>&1); rc=$?
if [ "$rc" -eq 2 ]; then
  echo 'PASS CLI  位置参数端口被拒绝'
else
  echo "FAIL CLI  位置参数端口没被拒绝（rc=$rc）：$out"; cli_fail=1
fi
[ "$cli_fail" -eq 0 ] || exit 1

unshare -rn bash -c "
  ip link set lo up
  python3 $S/ua_echo.py 80 >/dev/null 2>&1 &
  python3 $S/raw_echo.py 9201 >/dev/null 2>&1 &
  $GO_BIN -p 8898 >/dev/null 2>&1 &
  python3 $PY_SCRIPT 8897 >/dev/null 2>&1 &
  sleep 1.5
  echo '===== Go 版 (8898) ====='
  python3 $S/ua_test.py 8898 80 9201 --slow
  echo '===== Python 参考版 (8897) ====='
  python3 $S/ua_test.py 8897 80 9201
  kill %1 %2 %3 %4 2>/dev/null
"
