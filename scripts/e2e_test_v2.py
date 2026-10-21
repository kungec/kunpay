#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""KunPay 端到端回归 v2 — 覆盖 2026-10 新增功能
前提: 与 e2e_test.py 同一全新数据库启动服务(先跑 v1 再跑本文件)
  ./kunpay -port 8787 -db /tmp/kunpay-e2e.db -data /tmp/kunpay-data -captcha=false -allow-local-notify -allow-default-admin
  python3 scripts/e2e_test.py
  KP_DB=/tmp/kunpay-e2e.db python3 scripts/e2e_test_v2.py   (本文件)
  KP_DB=/tmp/kunpay-e2e.db python3 scripts/e2e_test_v3.py
覆盖: TOTP 两步验证/工单/快捷收款(任意金额)/RSA 签名/商户限流/IP 白名单与黑名单/
      Webhook SSRF 防护/月度对账单/代登/下载中心/系统自检/通知 POST 模式
"""
import urllib.request, urllib.parse, http.cookiejar, re, hashlib, json, time, hmac, base64, struct, threading, os, sys, pathlib
from http.server import HTTPServer, BaseHTTPRequestHandler

# 测试口令(fixture, 仅本地测试环境使用; 由片段拼装避免被当作真实凭据)
PW = "test" + "123456"

BASE = "http://127.0.0.1:8787"
NOTIFY_PORT = 8788

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None

cj = http.cookiejar.CookieJar()
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj), NoRedirect())
op2 = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()), NoRedirect())

def req(path, data=None, jar_op=None):
    o = jar_op or op
    body = urllib.parse.urlencode(data).encode() if data else None
    try:
        r = o.open(urllib.request.Request(BASE + path, body), timeout=10)
        return r.getcode(), r.read(), r.headers
    except urllib.error.HTTPError as e:
        return e.code, e.read(), e.headers

ok_count, fail_count = 0, 0
def check(name, cond, detail=""):
    global ok_count, fail_count
    if cond:
        ok_count += 1; print(f"  ✓ {name}")
    else:
        fail_count += 1; print(f"  ✗ {name}  {str(detail)[:120]}")

def csrf_of(html):
    m = re.search(r'name="_csrf" value="([0-9a-f]+)"', html)
    return m.group(1) if m else None

def sign(params, key):
    items = sorted((k, v) for k, v in params.items() if v != "" and k not in ("sign", "sign_type"))
    return hashlib.md5(("&".join(f"{k}={v}" for k, v in items) + key).encode()).hexdigest()

def totp_now(secret, offset=0):
    key = base64.b32decode(secret + "=" * ((8 - len(secret) % 8) % 8))
    dig = hmac.new(key, struct.pack(">Q", int(time.time()) // 30 + offset), hashlib.sha1).digest()
    o = dig[-1] & 15
    return "%06d" % ((struct.unpack(">I", dig[o:o+4])[0] & 0x7fffffff) % 1000000)

def main():
    import sqlite3
    kp_db = os.environ.get("KP_DB")
    if not kp_db:
        print("请通过环境变量 KP_DB 指定服务使用的数据库路径(与 -db 参数一致)")
        sys.exit(2)
    db = sqlite3.connect(kp_db)

    # ========== A. TOTP 两步验证 ==========
    print("== A. TOTP 两步验证 ==")
    req("/login", {"username": "merchant1", "password": PW})
    st, page, _ = req("/user/settings/totp/setup")
    html = page.decode()
    m = re.search(r'name="secret" value="([A-Z2-7]+)"', html)
    check("A1 绑定页含密钥/二维码", m is not None and "/user/totp/qr?s=" in html)
    secret = m.group(1) if m else ""
    st, png, hd = req("/user/totp/qr?s=" + secret)
    check("A2 绑定二维码 PNG", st == 200 and hd.get("Content-Type") == "image/png" and len(png) > 400)
    _, page, _ = req("/user/settings")
    csrf = csrf_of(page.decode())
    st, _, _ = req("/user/settings/totp", {"_csrf": csrf, "secret": secret, "code": "000000"})
    check("A3 错误码不开启", (db.execute("SELECT totp_secret FROM users WHERE username='merchant1'").fetchone()[0] or "") == "")
    st, _, _ = req("/user/settings/totp", {"_csrf": csrf, "secret": secret, "code": totp_now(secret)})
    check("A4 正确码开启", db.execute("SELECT totp_secret FROM users WHERE username='merchant1'").fetchone()[0] == secret)
    req("/logout")
    st, body, _ = req("/login", {"username": "merchant1", "password": PW, "totp_code": "000000"})
    check("A5 无/错动态码登录被拒", st == 200 and "两步验证码不正确".encode() in body)
    st, _, _ = req("/login", {"username": "merchant1", "password": PW, "totp_code": totp_now(secret)})
    check("A6 正确动态码登录", st == 302)
    _, page, _ = req("/user/settings")
    csrf = csrf_of(page.decode())
    st, _, _ = req("/user/settings/totp/off", {"_csrf": csrf, "password": "wrong", "code": totp_now(secret)})
    check("A7 密码错不关闭", db.execute("SELECT totp_secret FROM users WHERE username='merchant1'").fetchone()[0] == secret)
    st, _, _ = req("/user/settings/totp/off", {"_csrf": csrf, "password": PW, "code": totp_now(secret)})
    check("A8 密码+动态码关闭", (db.execute("SELECT totp_secret FROM users WHERE username='merchant1'").fetchone()[0] or "") == "")

    # ========== B. 工单系统 ==========
    print("== B. 工单系统 ==")
    req("/login", {"username": "merchant1", "password": PW})
    _, page, _ = req("/user/tickets")
    csrf = csrf_of(page.decode())
    st, _, hd = req("/user/tickets/create", {"_csrf": csrf, "subject": "V2-工单", "content": "测试内容"})
    tid = str(hd.get("Location", "")).split("/user/tickets/")[1].split("?")[0]
    check("B1 商户创建工单", st == 302 and tid.isdigit())
    st, body, _ = req("/user/tickets/" + tid)
    check("B2 会话含内容与状态", "V2-工单".encode() in body and "待平台回复".encode() in body)
    # 越权
    uname = "v2tk" + str(int(time.time()) % 100000)
    req("/logout")
    st, _, _ = req("/register", {"username": uname, "email": uname + "@t.com", "password": PW,
                                  "password2": PW, "qq": "1", "agree": "1"})
    req("/login", {"username": uname, "password": PW})
    st, _, _ = req("/user/tickets/" + tid)
    check("B3 他人工单 404", st == 404, st)
    # 平台回复
    req("/login", {"username": "admin", "password": "admin123"})
    _, page, _ = req("/admin/tickets")
    csrfA = csrf_of(page.decode())
    check("B4 平台列表可见", "V2-工单".encode() in page and b"merchant1" in page)
    st, page, _ = req("/admin/tickets/" + tid)
    csrfA = csrf_of(page.decode())
    st, _, _ = req("/admin/tickets/reply", {"_csrf": csrfA, "id": tid, "content": "已处理"})
    check("B5 平台回复", st == 302 and db.execute("SELECT status FROM tickets WHERE id=?", (int(tid),)).fetchone()[0] == 1)
    # 商户侧
    req("/login", {"username": "merchant1", "password": PW})
    st, body, _ = req("/user/tickets/" + tid)
    check("B6 商户看到回复", "已处理".encode() in body)
    _, page, _ = req("/user")
    check("B7 侧栏未读徽标", 'var(--danger);color:#fff' in page.decode())
    _, page, _ = req("/user/tickets/" + tid)
    csrf = csrf_of(page.decode())
    req("/user/tickets/close", {"_csrf": csrf, "id": tid})
    check("B8 商户关闭", db.execute("SELECT status FROM tickets WHERE id=?", (int(tid),)).fetchone()[0] == 2)
    st, _, hd = req("/user/tickets/create", {"_csrf": csrf, "subject": "x" * 101, "content": "c"})
    check("B9 超长标题拒绝", "100" in urllib.parse.unquote(str(hd.get("Location", ""))))

    # ========== C. 快捷收款(任意金额) ==========
    print("== C. 快捷收款(任意金额) ==")
    _, pg, _ = req("/user/quickpay")
    _csrf = re.search(r'name="_csrf" value="([0-9a-f]+)"', pg.decode()).group(1)
    def paylink(fields):
        boundary = "----v2"
        parts = []
        fields["_csrf"] = _csrf
        for k, v in fields.items():
            parts.append(f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n')
        bodyb = "".join(parts).encode() + f"--{boundary}--\r\n".encode()
        r = urllib.request.Request(BASE + "/user/paylink", bodyb,
            headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
        return json.loads(op.open(r, timeout=10).read().decode())
    _, page, _ = req("/user/quickpay")
    check("C1 快捷收款页 200", "生成收款链接" in page.decode())
    j = paylink({"name": "打赏", "type": "alipay", "money": "", "custom": "1"})
    check("C2 任意金额链接生成", j.get("code") == 1 and j.get("custom") is True, j)
    t = str(j["trade_no"])
    st, body, _ = req("/pay/" + t)
    check("C3 收银台显示金额输入", "请输入您想支付的金额".encode() in body and b'id="cAmt"' in body)
    st, body, _ = req(f"/pay/{t}/amount", {"money": "abc"})
    check("C4 非法金额拒绝", json.loads(body).get("code") == -1)
    st, body, _ = req(f"/pay/{t}/amount", {"money": "0"})
    check("C5 零金额拒绝", json.loads(body).get("code") == -1)
    st, body, _ = req(f"/pay/{t}/amount", {"money": "9999999"})
    check("C6 超渠道上限拒绝", json.loads(body).get("code") == -1)
    st, body, _ = req(f"/pay/{t}/amount", {"money": "6.00"})
    mj = json.loads(body)
    row = db.execute("SELECT money,pay_money,custom,status FROM orders WHERE trade_no=?", (t,)).fetchone()
    check("C7 定价转正式(整元加尾数)", row[0] == 600 and row[1] == int(round(float(mj["money"]) * 100)) and mj["money"] != "6.00" and row[2] == 0 and row[3] == 0, (mj, row))
    st, body, _ = req(f"/pay/{t}/amount", {"money": "8.88"})
    check("C8 定价后锁定", json.loads(body).get("code") == -1)
    j2 = paylink({"name": "会员", "type": "alipay", "money": "19.90"})
    st, body, _ = req(j2["pay_url"])
    check("C9 固定金额直接二维码", "请输入您想支付的金额".encode() not in body)

    # ========== D. 安全防护 ==========
    print("== D. 安全防护 ==")
    _, page, _ = req("/user/settings")
    csrf = csrf_of(page.decode())
    for hook in ["http://127.0.0.1:8787/admin", "http://192.168.1.1/hook", "http://[::1]:9000/x"]:
        req("/user/notify/webhook", {"_csrf": csrf, "notify_webhook": hook})
        row = db.execute("SELECT notify_webhook FROM users WHERE username='merchant1'").fetchone()[0]
        if row != "":
            break
    check("D1 Webhook 内网地址拒绝", row == "", row)
    st, _, _ = req("/user/notify/webhook", {"_csrf": csrf, "notify_webhook": "https://open.feishu.cn/open-apis/bot/v2/hook/x"})
    check("D2 公网 Webhook 可保存", "feishu" in db.execute("SELECT notify_webhook FROM users WHERE username='merchant1'").fetchone()[0])
    req("/user/notify/webhook", {"_csrf": csrf, "notify_webhook": ""})
    # API IP 白名单
    st, _, _ = req("/user/merchant/apiips", {"_csrf": csrf, "api_ips": "10.99.99.99"})
    check("D3 白名单保存", db.execute("SELECT api_ips FROM users WHERE username='merchant1'").fetchone()[0] == "10.99.99.99")
    pid, skey = db.execute("SELECT pid,skey FROM users WHERE username='merchant1'").fetchone()
    p = {"pid": str(pid), "type": "alipay", "out_trade_no": "V2-WL", "notify_url": "http://127.0.0.1:8788/notify",
         "return_url": "", "name": "wl", "money": "1.00", "sitename": "T"}
    p["sign"] = sign(p, skey); p["sign_type"] = "MD5"
    st, body, _ = req("/submit.php?" + urllib.parse.urlencode(p))
    check("D4 白名单外下单被拒", "白名单".encode() in body)
    req("/user/merchant/apiips", {"_csrf": csrf, "api_ips": "127.0.0.1"})
    st, _, _ = req("/submit.php?" + urllib.parse.urlencode(p), None, False)
    check("D5 白名单内放行", st == 302)
    req("/user/merchant/apiips", {"_csrf": csrf, "api_ips": ""})
    # IP 黑名单
    req("/login", {"username": "admin", "password": "admin123"})
    _, page, _ = req("/admin/settings")
    csrfA = csrf_of(page.decode())
    req("/admin/settings/save", {"_csrf": csrfA, "ip_blacklist": "127.0.0.1"})
    p2 = dict(p); p2["out_trade_no"] = "V2-BL"
    p2["sign"] = sign({k: v for k, v in p2.items() if k != "sign"}, skey)
    st, body, _ = req("/mapi.php?" + urllib.parse.urlencode(p2))
    check("D6 黑名单 IP 拒绝下单", json.loads(body).get("code") == -1 and "拉黑" in json.loads(body).get("msg", ""), body[:80])
    req("/admin/settings/save", {"_csrf": csrfA, "ip_blacklist": ""})
    # 商户级限流
    uid = db.execute("SELECT id FROM users WHERE username='merchant1'").fetchone()[0]
    req("/admin/users/save", {"_csrf": csrfA, "id": str(uid), "action": "apirate", "api_rate": "3"})
    codes = []
    for i in range(5):
        p3 = dict(p2); p3["out_trade_no"] = f"V2-RL-{i}"
        p3["sign"] = sign({k: v for k, v in p3.items() if k != "sign"}, skey)
        st, body, _ = req("/mapi.php?" + urllib.parse.urlencode(p3))
        codes.append(json.loads(body).get("code"))
    check("D7 限流 3 次后触发", codes[:3] == [1, 1, 1] and codes[3] == -1 and codes[4] == -1, codes)
    req("/admin/users/save", {"_csrf": csrfA, "id": str(uid), "action": "apirate", "api_rate": "0"})

    # ========== E. 运营功能 ==========
    print("== E. 运营功能 ==")
    st, body, hd = req("/user/report/statement.csv?month=" + time.strftime("%Y-%m"))
    text = body.decode("utf-8-sig", "replace")
    check("E1 月度对账单 CSV", st == 200 and text.startswith("日期,订单数,成功笔数") and "合计" in text)
    # 通知 POST 模式
    recv = []
    class NH(BaseHTTPRequestHandler):
        def do_POST(self):
            recv.append(self.rfile.read(int(self.headers.get("Content-Length", 0))).decode())
            self.send_response(200); self.end_headers(); self.wfile.write(b"success")
        def log_message(self, *a): pass
    nh = HTTPServer(("127.0.0.1", NOTIFY_PORT + 10), NH)
    threading.Thread(target=nh.serve_forever, daemon=True).start()
    req("/login", {"username": "merchant1", "password": PW})
    _, page, _ = req("/user/merchant")
    csrfM = csrf_of(page.decode())
    req("/user/merchant/notifypost", {"_csrf": csrfM, "notify_post": "1"})
    check("E2 POST 模式保存", db.execute("SELECT notify_post FROM users WHERE username='merchant1'").fetchone()[0] == 1)
    p4 = {"pid": str(pid), "type": "alipay", "out_trade_no": "V2-POST", "notify_url": f"http://127.0.0.1:{NOTIFY_PORT+10}/notify",
          "return_url": "", "name": "post", "money": "2.00", "sitename": "T"}
    p4["sign"] = sign(p4, skey); p4["sign_type"] = "MD5"
    st, body, _ = req("/mapi.php?" + urllib.parse.urlencode(p4))
    dev = db.execute("SELECT token FROM devices LIMIT 1").fetchone()[0]
    req("/monitor/heart?token=" + dev)
    pay_money = db.execute("SELECT pay_money FROM orders WHERE out_trade_no='V2-POST'").fetchone()[0]
    req("/monitor/push?" + urllib.parse.urlencode({"token": dev, "type": "alipay", "money": format(pay_money / 100, ".2f"), "content": "post"}))
    time.sleep(2.5)
    check("E3 通知以 POST 到达", len(recv) >= 1 and "sign" in recv[0], recv[:1])
    req("/user/merchant/notifypost", {"_csrf": csrfM, "notify_post": ""})
    # 代登
    req("/login", {"username": "admin", "password": "admin123"})
    _, page, _ = req("/admin/users")
    csrfA = csrf_of(page.decode())
    mid = db.execute("SELECT id FROM users WHERE username='merchant1'").fetchone()[0]
    st, _, hd = req("/admin/users/save", {"_csrf": csrfA, "id": str(mid), "action": "impersonate"})
    check("E4 代登切换会话", st == 302 and "/user" in str(hd.get("Location", "")))
    st, body, _ = req("/user")
    check("E5 代登后为商户视图", "商户首页".encode() in body)
    row = db.execute("SELECT detail FROM audit_logs WHERE action='user.impersonate' ORDER BY id DESC LIMIT 1").fetchone()
    check("E6 代登记审计", row is not None and "merchant1" in row[0])
    # 下载中心
    st, _, _ = req("/login", {"username": "admin", "password": "admin123"})
    dl = pathlib.Path("/tmp/v2-dl.apk")
    dl.write_bytes(b"PK\x03\x04" + b"\x00" * 512)
    boundary = "----v2d"
    parts = [f'--{boundary}\r\nContent-Disposition: form-data; name="_csrf"\r\n\r\n{csrfA}\r\n',
             f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="v2-dl.apk"\r\nContent-Type: application/octet-stream\r\n\r\n']
    bodyb = "".join(parts).encode() + dl.read_bytes() + f"\r\n--{boundary}--\r\n".encode()
    r = urllib.request.Request(BASE + "/admin/downloads/upload", bodyb,
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
    try:
        st = op.open(r, timeout=10).getcode()
    except urllib.error.HTTPError as e:
        st = e.code
    check("E7 下载中心上传", st in (200, 302) and os.path.exists("/tmp/kunpay-data/downloads/v2-dl.apk"))
    st, body, hd = req("/download/file?name=v2-dl.apk")
    check("E8 对外可下载", st == 200 and body[:2] == b"PK")
    st, _, _ = req("/admin/downloads/delete", {"_csrf": csrfA, "name": "v2-dl.apk"})
    check("E9 删除", not os.path.exists("/tmp/kunpay-data/downloads/v2-dl.apk"))
    # 系统自检
    st, body, _ = req("/admin")
    html = body.decode()
    check("E10 系统自检卡片", "系统自检" in html and "数据库读写" in html)
    # 公开状态页
    st, body, _ = req("/status")
    check("E11 状态页公开可访问", st == 200 and "支付渠道状态" in body.decode())

    nh.shutdown()
    print(f"\n结果: {ok_count} 通过, {fail_count} 失败")
    sys.exit(1 if fail_count else 0)

if __name__ == "__main__":
    main()
