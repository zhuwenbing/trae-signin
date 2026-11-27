#!/usr/bin/env bash
# signin.sh — TRAE 批量签到脚本，签到后通过 Server酱 / Bark 推送通知
set -e
cd "$(dirname "$0")"

# Server酱 SendKey：export SERVERCHAN_SENDKEY='SCT...'
# Server酱³ 等自定义端点：export SERVERCHAN_API='https://<uid>.push.ft07.com/send'
# Bark 推送地址：export BARK_URL='https://api.day.app/你的Key'
# 两个都配置时会同时推送；只配置一个就只推一个。
SERVERCHAN_SENDKEY="${SERVERCHAN_SENDKEY:-${SC_KEY:-}}"
SERVERCHAN_API="${SERVERCHAN_API:-https://sctapi.ftqq.com}"
SERVERCHAN_API="${SERVERCHAN_API%/}"
BARK_URL="${BARK_URL:-}"
BARK_URL="${BARK_URL%/}"

# 对路径/正文做百分号编码（逐字节，兼容 UTF-8；Bark GET 接口需要）
urlencode() {
  printf '%s' "$1" | od -An -tx1 | tr -d ' \n' | sed 's/../%&/g'
}

# 已交叉编译好 signin_bin 时直接复用（适合只部署、不装 Go 的 Linux 机器 / Docker 镜像）；
# 源码比二进制新时才重新编译。
if [ -z "${SIGNIN_BIN:-}" ]; then
  if [ ! -x ./signin_bin ] || [ -n "$(find cmd internal go.mod -newer ./signin_bin -print -quit 2>/dev/null)" ]; then
    go build -o signin_bin ./cmd/signin
  fi
  SIGNIN_BIN=./signin_bin
fi
[ -x "$SIGNIN_BIN" ] || { echo "❌ 找不到可执行的 $SIGNIN_BIN" >&2; exit 1; }

# 执行签到并捕获输出（签到程序退出码不影响后续通知）
set +e
SIGNIN_OUTPUT=$("$SIGNIN_BIN" "${1:-auths}" 2>&1)
SIGNIN_EXIT=$?
set -e

echo "$SIGNIN_OUTPUT"

# 提取关键信息用于 Server酱 推送
TOTAL=$(echo "$SIGNIN_OUTPUT" | grep -oP '总计=\K\d+' || echo "?")
OK=$(echo "$SIGNIN_OUTPUT" | grep -oP '签到成功=\K\d+' || echo "?")
ALREADY=$(echo "$SIGNIN_OUTPUT" | grep -oP '已签=\K\d+' || echo "?")
# 禁用：服务端返回签到活动未开启（enable=false），不计入失败但要显示，否则总数对不上
DISABLED=$(echo "$SIGNIN_OUTPUT" | grep -oP '禁用=\K\d+' || echo "0")
DISABLED=${DISABLED:-0}
FAIL=$(echo "$SIGNIN_OUTPUT" | grep -oP '失败=\K\d+' || echo "?")
# 未确认：接口返回成功但未真正到账（疑似风控），不计入失败但要提示
UNCONF=$(echo "$SIGNIN_OUTPUT" | grep -oP '未确认=\K\d+' || echo "0")
UNCONF=${UNCONF:-0}
# 设备异常：设备号未被服务端认可（code=9074）
BUSY=$(echo "$SIGNIN_OUTPUT" | grep -oP '设备异常=\K\d+' || echo "0")
BUSY=${BUSY:-0}

# 提取每个账号的信息（表格列：UID | 昵称 | 状态 | 总计 | 月赠 | 签到 | 详情）
ACCOUNTS=""
while IFS= read -r line; do
    if echo "$line" | grep -qP '│\s+\d+'; then
        nick=$(echo "$line" | sed 's/│/\n/g' | sed -n '3p' | xargs)
        status=$(echo "$line" | sed 's/│/\n/g' | sed -n '4p' | xargs)
        total=$(echo "$line" | sed 's/│/\n/g' | sed -n '5p' | xargs)
        chk=$(echo "$line" | sed 's/│/\n/g' | sed -n '7p' | xargs)
        ACCOUNTS="${ACCOUNTS}${nick} ${status} 总${total}(签到${chk})"$'\n'
    fi
done <<< "$SIGNIN_OUTPUT"

# 构建 Server酱 推送内容
TITLE="TRAE 签到"
if [ "$BUSY" -gt 0 ] 2>/dev/null; then
    TITLE="🔑 TRAE 设备号异常"
elif [ "$UNCONF" -gt 0 ] 2>/dev/null; then
    TITLE="⚠️ TRAE 签到未确认"
elif [ "$SIGNIN_EXIT" -eq 0 ]; then
    if [ "$OK" -gt 0 ] 2>/dev/null; then
        TITLE="✅ TRAE 签到成功"
    elif [ "$FAIL" -gt 0 ] 2>/dev/null; then
        TITLE="⚠️ TRAE 签到异常"
    else
        TITLE="📌 TRAE 已签到"
    fi
else
    TITLE="❌ TRAE 签到失败"
fi

BODY="总计${TOTAL} | 成功${OK} | 已签${ALREADY} | 禁用${DISABLED} | 未确认${UNCONF} | 设备异常${BUSY} | 失败${FAIL}"$'\n'"${ACCOUNTS}"

# 发送通知：Server酱 / Bark 可单独启用，也可同时启用
SENT=0
if [ -n "$SERVERCHAN_SENDKEY" ]; then
  if curl -fsS -X POST "$SERVERCHAN_API/$SERVERCHAN_SENDKEY.send" \
      --data-urlencode "title=$TITLE" \
      --data-urlencode "desp=$BODY" >/dev/null 2>&1; then
    echo "📲 Server酱 通知已发送"
  else
    echo "⚠️ Server酱 推送失败，请检查 SERVERCHAN_SENDKEY / SERVERCHAN_API"
  fi
  SENT=1
fi

if [ -n "$BARK_URL" ]; then
  if curl -fsS "$BARK_URL/$(urlencode "$TITLE")/$(urlencode "$BODY")" >/dev/null 2>&1; then
    echo "📲 Bark 通知已发送"
  else
    echo "⚠️ Bark 推送失败，请检查 BARK_URL"
  fi
  SENT=1
fi

if [ "$SENT" -eq 0 ]; then
  echo "📲 未配置 SERVERCHAN_SENDKEY / BARK_URL，跳过推送"
fi
