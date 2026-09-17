#!/bin/sh
# CN 侧（workbuddy.cn）邀请码绑定 —— 与 global 版 bind_invite.sh 对称。
#
# 用法:
#   sh bind_invite_cn.sh                        # 全部 cn 账号，默认码 evaxd5iz16ln
#   sh bind_invite_cn.sh evaxd5iz16ln           # 指定邀请码
#   sh bind_invite_cn.sh evaxd5iz16ln 红花       # 只处理某个昵称
#   sh bind_invite_cn.sh --my-code              # 只读：查每个 cn 账号自己的邀请码
#   BASE=https://www.workbuddy.cn sh bind_invite_cn.sh ...   # 覆盖域名
#
# 奖励口径（用户提供，2026-09-17）：
#   基础奖  50 积分：好友**首次使用**即得
#   活跃奖 100 积分：好友 **7 日内累计使用 3 天**额外得
#   → 与 global 一样，"绑定"只是前置；奖励要等好友真正使用才结算。
#
# 返回码语义（与 global 同一套）:
#   0                        成功绑定
#   12301/12306/12314/12319  邀请码无效
#   12302/12303/12310/12313  静默（已绑过 / 不能用自己的码 → 说明该号就是码主）
#   12304/12311              仅新用户可用
#   12305/12312              活动已结束
#   401（APISIX）            拿错 realm 的 token 打错域（cn 码只能配 cn token + workbuddy.cn）
#
# ⚠️ token 全程只在 NAS 本地/容器内流转，不落盘、不外传。
CODE="${1:-evaxd5iz16ln}"
ONLY="$2"
BASE="${BASE:-https://www.workbuddy.cn}"
DIR=$HOME/docker/workbuddy2api-panel/auths
cd "$DIR" || exit 1

MYCODE=0
[ "$CODE" = "--my-code" ] && { MYCODE=1; ONLY="$2"; }

printf '%s\t%s\t%s\t%s\t%s\n' "nickname" "realm" "uid" "code" "message"
for f in *.json; do
  # 文件属主 10001，yeying 读不到 → 借容器读（容器内挂载同名路径）
  J=$(docker exec workbuddy2api cat "/app/auths/$f" 2>/dev/null)
  [ -z "$J" ] && continue
  N=$(printf '%s' "$J" | jq -r '.account.nickname // "?"')
  RL=$(printf '%s' "$J" | jq -r '.auth.realm // "cn"')
  [ "$RL" = "cn" ] || continue          # 本脚本只处理 cn 域账号
  [ -n "$ONLY" ] && [ "$N" != "$ONLY" ] && continue
  U=$(printf '%s' "$J" | jq -r '.account.uid // "?"')
  T=$(printf '%s' "$J" | jq -r '.auth.accessToken // ""')
  [ -z "$T" ] && { printf '%s\t%s\t%s\t%s\t%s\n' "$N" "$RL" "$U" "-1" "no accessToken"; continue; }

  if [ "$MYCODE" = "1" ]; then
    R=$(curl -s --max-time 30 \
        -H "Authorization: Bearer $T" \
        -H "Origin: $BASE" -H "Referer: $BASE/" \
        "$BASE/activity/workbuddy/invitation/v2/my-code" 2>/dev/null)
  else
    R=$(curl -s --max-time 30 -X POST \
        "$BASE/activity/workbuddy/invitation/v2/bind" \
        -H "Authorization: Bearer $T" \
        -H "Content-Type: application/json" \
        -H "Origin: $BASE" -H "Referer: $BASE/" \
        -d "{\"inviteCode\":\"$CODE\"}" 2>/dev/null)
  fi

  C=$(printf '%s' "$R" | jq -r '.code' 2>/dev/null)
  if [ "$MYCODE" = "1" ]; then
    M=$(printf '%s' "$R" | jq -r '.data.invite_code // .data.inviteCode // .data.message // .msg // "?"' 2>/dev/null)
  else
    M=$(printf '%s' "$R" | jq -r '.data.message // .msg // "?"' 2>/dev/null)
  fi
  printf '%s\t%s\t%s\t%s\t%s\n' "$N" "$RL" "$U" "${C:-ERR}" "$M"
  sleep 1
done
