#!/usr/bin/env bash
#
# 行为对等测试：Go 版 vs Python 版 steam-ua-proxy。
#
# 在独立的 user+net namespace 里跑，这样普通用户也能监听 80 端口，
# 覆盖「CONNECT 到 80 端口后在隧道内改写 UA」这条关键路径。
#
# 用法：scripts/run-tests.sh [go二进制] [python脚本]
set -u

ROOT=$(cd "$(dirname "$0")/.." && pwd)
GO_BIN=${1:-$ROOT/go/steam-ua-proxy}
PY_SCRIPT=${2:-$ROOT/python/steam-ua-proxy.py}
S=${ROOT}/scripts

unshare -rn bash -c "
  ip link set lo up
  python3 $S/ua_echo.py 80 >/dev/null 2>&1 &
  python3 $S/raw_echo.py 9201 >/dev/null 2>&1 &
  $GO_BIN 8898 >/dev/null 2>&1 &
  python3 $PY_SCRIPT 8897 >/dev/null 2>&1 &
  sleep 1.5
  echo '===== Go 版 (8898) ====='
  python3 $S/ua_test.py 8898 80 9201 --slow
  echo '===== Python 版 (8897) ====='
  python3 $S/ua_test.py 8897 80 9201
  kill %1 %2 %3 %4 2>/dev/null
"
