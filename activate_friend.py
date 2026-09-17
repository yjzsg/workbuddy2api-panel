#!/usr/bin/env python3
"""让「被邀请的好友」进入已使用（激活）状态，从而给邀请人结算邀请奖励。

背景（2026-09-16 实测）：
  邀请活动的「激活」判据不是账号发过 API 请求，而是 **官方客户端的行为事件链**。
  仅发一条 chat_request_send 上报无效；必须发**桌面端指纹的六事件链**到 /v2/report：
    agent_task_created → chat_message_send → chat_request_send →
    chat_message_response(isSuccessful=true) → chat_message_status → chat_request_response
  发完立即生效：invite-records 里该好友 activated=true / status=已使用…，
  邀请人 +100 积分（Bonus Pack 100），好友自己也 +100（约 1 分钟内到账）。

用法（在 fnOS 上，账号凭据在容器 /app/auths/）：
    python3 activate_friend.py <nickname|uid|all>
    python3 activate_friend.py all            # 处理 auths/ 下全部账号
"""
import hashlib
import json
import subprocess
import sys
import os
import time
import urllib.error
import urllib.request

HOST = os.environ.get("WB_HOST", "https://www.workbuddy.ai")   # CN 侧用 WB_HOST=https://www.workbuddy.cn
UA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
AUTHS_DIR = "/app/auths"


def list_accounts():
    out = subprocess.check_output(
        ["docker", "exec", "workbuddy2api", "sh", "-c", "ls " + AUTHS_DIR]).decode()
    return [x for x in out.split() if x.endswith(".json")]


def load(fname):
    raw = subprocess.check_output(
        ["docker", "exec", "workbuddy2api", "cat", AUTHS_DIR + "/" + fname])
    d = json.loads(raw)
    acct = d.get("account") or {}
    return acct.get("uid", ""), acct.get("nickname", ""), (d.get("auth") or {}).get("accessToken", "")


def derive(uid, salt):
    return hashlib.sha256((salt + ":" + uid).encode()).hexdigest()[:36]


def build_chain(uid, nick):
    now = int(time.time() * 1000)
    cid = derive(uid, "session")
    rid = "req" + str(now)
    mid = "msg" + str(now)
    model_id, model_name = "deepseek-v4-flash", "DeepSeek V4 Flash"
    fp = {
        "timezone": "Asia/Shanghai", "reportDelay": 2000,
        "userId": uid, "username": nick, "userNickname": nick,
        "product": "SaaS", "releaseDate": 1789036585355,
        "commit": "5f9692923c93033111c51ad7b003eb80204a9b75",
        "ideName": "WorkBuddy", "ideType": "WorkBuddy", "ideVersion": "5.5.6",
        "machineId": derive(uid, "machine"), "sessionId": derive(uid, "session"),
        "extName": "workbuddy-desktop", "extVersion": "5.5.6",
        "os": "win32", "arch": "x64", "osVersion": "10.0.26220",
        "cpuCores": 20, "memorySize": 24,
    }

    def ev(code, extra):
        m = dict(fp)
        m["timestamp"] = now
        m["presentAt"] = now
        m["eventCode"] = code
        m.update(extra)
        return m

    return [
        ev("agent_task_created", {
            "source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
            "requestModelId": model_id, "requestModelName": model_name,
            "has_repo": False, "repo_type": "none", "workspace_type": "empty",
            "has_connector": False, "connector_types": [],
            "has_mention": False, "mention_types": [],
            "has_template": False, "action": "", "template_name": "",
            "has_expert": False, "expert_id": "", "expert_name": "", "expert_industry_id": "",
            "has_skill": False, "skill_names": [],
            "conversationId": cid, "messageId": mid, "buddyId": "", "buddyName": "",
        }),
        ev("chat_message_send", {
            "messageId": mid + "-assistant", "historyCount": 0,
            "isContextTruncated": False, "currentStepCount": 1,
            "traceId": rid, "rootRequestId": rid, "parentConversationId": cid,
            "agentName": "cli", "agentType": "main",
        }),
        ev("chat_request_send", {
            "inputLength": 24, "isPlan": False, "isAutoExecuteTerminal": False,
            "isAutoModify": False, "codebaseEnable": False, "maxToken": 0,
            "maxSteps": 500, "temperature": 0, "maxRetries": 0,
            "mentionContexts": [], "knowledgeId": [], "knowledgeName": [],
            "codebaseId": "", "mentionContextCount": 0, "command": "",
            "recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
            "traceId": rid, "rootRequestId": rid, "parentConversationId": cid,
            "agentName": "cli", "agentType": "main",
            "codebuddy.session_id": cid, "codebuddy.conversation_request_id": rid,
        }),
        ev("chat_message_response", {
            "messageId": mid + "-assistant", "responseModelId": model_id,
            "inputToken": 120, "outputToken": 80, "totalToken": 200,
            "cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
            "isSuccessful": True, "messageErrorCode": "", "finishReason": "stop",
            "firstTokenAt": now, "traceId": rid, "conversationId": cid,
            "rootRequestId": rid, "parentConversationId": cid,
            "agentName": "cli", "agentType": "main",
            "codebuddy.session_id": cid, "codebuddy.conversation_request_id": rid,
        }),
        ev("chat_message_status", {
            "messageId": mid + "-assistant", "messageErrorCode": "0",
            "traceId": rid, "rootRequestId": rid, "parentConversationId": cid,
            "agentName": "cli", "agentType": "main",
        }),
        ev("chat_request_response", {
            "mode": "craft", "toolCallCount": 0,
            "inputToken": 120, "outputToken": 80, "totalToken": 200,
            "cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
            "isSuccessful": True, "messageErrorCode": "", "finishReason": "stop",
            "rootRequestId": rid, "parentConversationId": cid,
        }),
    ]


def send(uid, nick, tok):
    body = json.dumps(build_chain(uid, nick), ensure_ascii=False).encode()
    req = urllib.request.Request(HOST + "/v2/report", data=body, method="POST")
    for k, v in {
        "Authorization": "Bearer " + tok,
        "Accept": "application/json, text/plain, */*",
        "Content-Type": "application/json;charset=UTF-8",
        "User-Agent": UA,
        "X-Domain": HOST,
        "X-Product": "SaaS",
        "X-User-Id": uid,
        "X-Request-ID": derive(uid, "req") + str(time.time_ns() % 1000000),
        "Origin": HOST,
        "Referer": HOST + "/",
    }.items():
        req.add_header(k, v)
    try:
        r = urllib.request.urlopen(req, timeout=30)
        return "HTTP %s %s" % (r.status, r.read().decode()[:120])
    except urllib.error.HTTPError as e:
        return "HTTP %s %s" % (e.code, e.read().decode()[:120])
    except Exception as e:
        return "ERR %s %s" % (type(e).__name__, str(e)[:120])


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(1)
    want = sys.argv[1]
    n = 0
    for fn in list_accounts():
        uid, nick, tok = load(fn)
        if not uid or not tok:
            continue
        if want != "all" and want not in (nick, uid):
            continue
        print("%-24s %s" % (nick or fn, send(uid, nick, tok)))
        n += 1
        time.sleep(1)
    print("完成，处理 %d 个账号" % n)


if __name__ == "__main__":
    main()
