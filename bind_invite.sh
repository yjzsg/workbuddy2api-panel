#!/bin/sh
# 为 auths/ 下的账号绑定邀请码（幂等：已绑的返回 12310，无副作用）
#
# 用法:
#   sh bind_invite.sh                      # 全部账号，默认码 6G9QTTY3
#   sh bind_invite.sh 6G9QTTY3             # 指定邀请码
#   sh bind_invite.sh 6G9QTTY3 账号D   # 只处理某个昵称（逐个入库时用）
#
# 输出: nickname<TAB>uid<TAB>code<TAB>message
#
# 返回码语义（前端 te() 映射，实测）:
#   0                     成功绑定（受邀方白得积分；实测半残号 0 → 350）
#   12301/12306/12314/12319  邀请码无效
#   12302/12303/12310/12313  静默（已绑过 / 不能用自己的码）
#   12304/12311              仅新用户可用
#   12305/12312              活动已结束
#
# ⚠️ token 全程只在 NAS 本地/容器内流转，不落盘、不外传。
CODE="${1:-6G9QTTY3}"
ONLY="$2"
DIR=$HOME/docker/workbuddy2api-panel/auths
cd "$DIR" || exit 1

for f in *.json; do
  # 文件属主 10001，yeying 读不到 → 借容器读（容器内挂载同名路径）
  J=$(docker exec workbuddy2api cat "/app/auths/$f" 2>/dev/null)
  [ -z "$J" ] && continue
  N=$(printf '%s' "$J" | jq -r '.account.nickname // "?"')
  [ -n "$ONLY" ] && [ "$N" != "$ONLY" ] && continue
  U=$(printf '%s' "$J" | jq -r '.account.uid // "?"')
  T=$(printf '%s' "$J" | jq -r '.auth.accessToken // ""')
  [ -z "$T" ] && { printf '%s\t%s\t%s\t%s\n' "$N" "$U" "-1" "no accessToken"; continue; }

  R=$(curl -s --max-time 30 -X POST \
      "https://www.workbuddy.ai/activity/workbuddy/invitation/v2/bind" \
      -H "Authorization: Bearer $T" \
      -H "Content-Type: application/json" \
      -H "Origin: https://www.workbuddy.ai" \
      -H "Referer: https://www.workbuddy.ai/" \
      -d "{\"inviteCode\":\"$CODE\"}" 2>/dev/null)

  C=$(printf '%s' "$R" | jq -r '.code' 2>/dev/null)
  M=$(printf '%s' "$R" | jq -r '.data.message // .msg // "?"' 2>/dev/null)
  printf '%s\t%s\t%s\t%s\n' "$N" "$U" "${C:-ERR}" "$M"
  sleep 1
done
