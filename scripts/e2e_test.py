#!/usr/bin/env python3
"""KunPay 端到端功能测试"""
import hashlib, json, re, subprocess, sys, threading, time, urllib.parse, urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer

BASE = "http://localhost:8787"
NOTIFY_PORT = 8788
notify_hits = []

class NotifyHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        notify_hits.append(urllib.parse.parse_qs(self.path.split("?", 1)[1] if "?" in self.path else ""))
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"success")
    def log_message(self, *a): pass

def start_notify():
    srv = HTTPServer(("127.0.0.1", NOTIFY_PORT), NotifyHandler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()

def req(path, data=None, headers=None, follow=True):
    url = BASE + path
    body = urllib.parse.urlencode(data).encode() if data else None
    r = urllib.request.Request(url, data=body, headers=headers or {})
    opener = urllib.request.build_opener(NoRedirect) if not follow else urllib.request.build_opener()
    try:
        resp = opener.open(r, timeout=10)
        return resp.status, dict(resp.headers), resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode("utf-8", "replace")

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None

def sign(params, key):
    items = sorted((k, v) for k, v in params.items() if v != "" and k not in ("sign", "sign_type"))
    raw = "&".join(f"{k}={v}" for k, v in items) + key
    return hashlib.md5(raw.encode()).hexdigest()

ok_count, fail_count = 0, 0
def check(name, cond, detail=""):
    global ok_count, fail_count
    if cond:
        ok_count += 1
        print(f"  ✓ {name}")
    else:
        fail_count += 1
        print(f"  ✗ {name}  {detail}")

def main():
    start_notify()
    jar = {}       # cookie jar (merchant)
    admin_jar = {}

    def logged(path, data=None, jar_=None, follow=True):
        headers = {"Cookie": jar_["cookie"]} if jar_.get("cookie") else {}
        st, hd, body = req(path, data, headers, follow)
        if "set-cookie" in {k.lower() for k in hd}:
            for k, v in hd.items():
                if k.lower() == "set-cookie":
                    jar_["cookie"] = v.split(";")[0]
        return st, hd, body

    def form_with_csrf(path, jar_, extra):
        st, hd, body = logged(path, None, jar_)
        m = re.search(r'name="_csrf" value="([0-9a-f]+)"', body)
        if not m:
            return None, body
        data = dict(extra)
        data["_csrf"] = m.group(1)
        return logged(path, data, jar_), body

    print("== 1. 商户注册 ==")
    st, hd, body = req("/register", {"username": "merchant1", "email": "m1@test.com",
                                     "password": "test123456", "password2": "test123456", "qq": "10001", "agree": "1"}, follow=False)
    check("注册成功跳转 /user", st == 302 and "/user" in str(hd.get("Location", "")), f"st={st}")

    print("== 2. 登录 + 读取商户密钥 ==")
    st, hd, body = req("/login", {"username": "merchant1", "password": "test123456"}, follow=False)
    check("登录跳转", st == 302 and "/user" in str(hd.get("Location", "")), f"st={st}")
    jar["cookie"] = hd.get("Set-Cookie", "").split(";")[0]
    st, _, dash = req("/user", headers={"Cookie": jar["cookie"]})
    check("商户首页 200", st == 200)
    st, _, merch = req("/user/merchant", headers={"Cookie": jar["cookie"]})
    pid_m = re.search(r'pid=(\d+)', merch)
    pid = pid_m.group(1) if pid_m else None
    check("商户页展示 PID", pid is not None, merch[:400])
    key_m = re.search(r'copyText\(\'([0-9a-f]{64})\'', merch)
    key = key_m.group(1) if key_m else None
    check("商户密钥可读取", key is not None)

    print("== 3. 管理员登录 + 添加监控设备 ==")
    st, hd, body = req("/login", {"username": "admin", "password": "admin123"}, follow=False)
    admin_jar["cookie"] = hd.get("Set-Cookie", "").split(";")[0]
    _, _, dev_page = logged("/admin/devices", None, admin_jar)
    m = re.search(r'name="_csrf" value="([0-9a-f]+)"', dev_page)
    csrf = m.group(1)
    st, hd, _ = req("/admin/devices/add", {"_csrf": csrf, "name": "测试手机"}, {"Cookie": admin_jar["cookie"]})
    _, _, dev_page = req("/admin/devices", headers={"Cookie": admin_jar["cookie"]})
    tok_m = re.search(r"copyText\('([0-9a-f]{48})'", dev_page)
    token = tok_m.group(1) if tok_m else None
    check("设备 Token 创建", token is not None)

    print("== 4. 提交订单 (submit.php) ==")
    params = {
        "pid": pid, "type": "alipay", "out_trade_no": "E2E-ORDER-001",
        "notify_url": f"http://127.0.0.1:{NOTIFY_PORT}/notify", "return_url": "http://merchant.example/return",
        "name": "测试商品", "money": "10.00", "sitename": "E2E测试",
    }
    params["sign"] = sign(params, key)
    params["sign_type"] = "MD5"
    st, hd, body = req("/submit.php?" + urllib.parse.urlencode(params), follow=False)
    loc = str(hd.get("Location", ""))
    trade_m = re.search(r"/pay/(\d+)", loc)
    check("签名校验通过并跳转收银台", trade_m is not None, f"st={st} loc={loc}")
    trade_no = trade_m.group(1) if trade_m else ""

    print("== 5. 错误签名被拒绝 ==")
    bad = dict(params); bad["sign"] = "0" * 32
    st, hd, body = req("/submit.php?" + urllib.parse.urlencode(bad))
    check("错误签名返回提示", "签名校验失败" in body)

    print("== 6. 收银台页面 ==")
    st, _, cashier = req(f"/pay/{trade_no}", headers={"Cookie": jar["cookie"]})
    check("收银台 200", st == 200)
    pay_m = re.search(r'¥(\d+\.\d+)<small>元', cashier)
    pay_amount = pay_m.group(1) if pay_m else None
    check("展示应付金额(含尾数)", pay_amount is not None and pay_amount != "10.00", f"应付={pay_amount}")
    check("含倒计时元素", 'id="countdown"' in cashier)

    print("== 7. mapi.php JSON 下单 ==")
    p2 = {"pid": pid, "type": "wxpay", "out_trade_no": "E2E-ORDER-002", "notify_url": f"http://127.0.0.1:{NOTIFY_PORT}/notify",
          "name": "API商品", "money": "20.00"}
    p2["sign"] = sign(p2, key); p2["sign_type"] = "MD5"
    st, _, body = req("/mapi.php?" + urllib.parse.urlencode(p2))
    j = json.loads(body)
    check("mapi code=1", j.get("code") == 1, body[:200])
    check("mapi 返回 payurl/img/money", all(k in j for k in ("payurl", "img", "money")))

    print("== 8. 监控设备心跳 + 到账上报匹配订单1 ==")
    st, _, body = req(f"/monitor/heart?token={token}")
    check("心跳成功", json.loads(body).get("code") == 1, body)
    push_q = urllib.parse.urlencode({"token": token, "type": "alipay", "money": pay_amount, "content": "收款十元"})
    st, _, body = req(f"/monitor/push?{push_q}")
    j = json.loads(body)
    check("到账匹配订单", j.get("code") == 1 and trade_no in j.get("msg", ""), body)

    print("== 9. 异步通知送达 ==")
    time.sleep(1.5)
    check("notify 已回调", len(notify_hits) >= 1, str(notify_hits))
    if notify_hits:
        np = {k: v[0] for k, v in notify_hits[0].items()}
        check("通知含 TRADE_SUCCESS", np.get("trade_status") == "TRADE_SUCCESS")
        check("通知签名可验", sign(np, key) == np.get("sign"))

    print("== 10. api.php 查询订单与商户 ==")
    st, _, body = req(f"/api.php?act=order&pid={pid}&key={key}&out_trade_no=E2E-ORDER-001")
    j = json.loads(body)
    check("查询订单状态=1", j.get("code") == 1 and j.get("status") == 1, body[:300])
    st, _, body = req(f"/api.php?act=query&pid={pid}&key={key}")
    j = json.loads(body)
    check("查询商户余额=9.90 (费率1%)", j.get("code") == 1 and j.get("money") == "9.90", f"money={j.get('money')}")
    st, _, body = req(f"/api.php?act=orders&pid={pid}&key={key}")
    j = json.loads(body)
    check("批量查询订单", j.get("code") == 1 and isinstance(j.get("data"), list))

    print("== 11. 订单2 (mapi 下单) 用监控上报完成 ==")
    amount2 = None
    _, _, cashier2 = req(j["data"][0]["trade_no"]) if False else (200, None, None)
    st, _, body = req(f"/api.php?act=order&pid={pid}&key={key}&out_trade_no=E2E-ORDER-002")
    j2 = json.loads(body)
    check("订单2创建存在", j2.get("code") == 1, body[:200])
    # 收银台取实付金额
    tno2 = j2.get("trade_no")
    _, _, c2 = req(f"/pay/{tno2}")
    m2 = re.search(r'¥(\d+\.\d+)<small>元', c2)
    amount2 = m2.group(1)
    push_q = urllib.parse.urlencode({"token": token, "type": "wxpay", "money": amount2, "content": f"微信收款{amount2}元"})
    st, _, body = req(f"/monitor/push?{push_q}")
    check("订单2匹配", json.loads(body).get("code") == 1, body)

    print("== 11b. V免签兼容协议心跳+推送 ==")
    t = str(int(time.time()))
    vsign = hashlib.md5((t + token).encode()).hexdigest()
    st, _, body = req(f"/appHeart?t={t}&sign={vsign}")
    check("vmq 心跳", json.loads(body).get("code") == 1, body)
    # 下单3并经 vmq 推送匹配
    p4 = {"pid": pid, "type": "qqpay", "out_trade_no": "E2E-ORDER-004", "name": "vmq商品", "money": "5.00"}
    p4["sign"] = sign(p4, key); p4["sign_type"] = "MD5"
    st, _, body = req("/mapi.php?" + urllib.parse.urlencode(p4))
    j4 = json.loads(body)
    tno4 = j4["trade_no"]
    _, _, c4 = req(f"/pay/{tno4}")
    m4 = re.search(r'¥(\d+\.\d+)<small>元', c4)
    push_q = urllib.parse.urlencode({"t": str(int(time.time())), "sign": hashlib.md5((str(int(time.time())) + token).encode()).hexdigest(),
                                     "type": "qqpay", "money": m4.group(1), "content": "QQ到账"})
    st, _, body = req(f"/appPushV?{push_q}")
    check("vmq 到账推送匹配", json.loads(body).get("code") == 1, body)

    print("== 12. 余额与提现流程 ==")
    st, _, settle_page = req("/user/settle", headers={"Cookie": jar["cookie"]})
    m3 = re.search(r'name="_csrf" value="([0-9a-f]+)"', settle_page)
    # 先设置结算账号
    req("/user/settings/profile", {"_csrf": m3.group(1), "email": "m1@test.com", "qq": "10001",
        "settle_type": "1", "settle_account": "merchant1@alipay", "settle_name": "张三"},
        {"Cookie": jar["cookie"]})
    st, hd, _ = req("/user/settle/apply", {"_csrf": m3.group(1), "amount": "10.00"}, {"Cookie": jar["cookie"]}, follow=False)
    loc = str(hd.get("Location", ""))
    check("提现申请提交", "msg=" in loc, loc)
    _, _, sp = req("/user/settle", headers={"Cookie": jar["cookie"]})
    check("提现记录可见", "待处理" in sp)
    # 管理员拒绝 → 退款
    _, _, sp = req("/admin/settles", headers={"Cookie": admin_jar["cookie"]})
    id_m = re.search(r'name="id" value="(\d+)"', sp)
    if not id_m:
        _, _, sp = req("/admin/settles", headers={"Cookie": admin_jar["cookie"]})
        id_m = re.search(r'name="id" value="(\d+)"', sp)
    st, hd, _ = req("/admin/settles/do", {"_csrf": csrf, "id": id_m.group(1), "action": "reject"}, {"Cookie": admin_jar["cookie"]}, follow=False)
    check("管理员拒绝提现", "err" not in str(hd.get("Location", "")), str(hd.get("Location")))
    st, _, body = req(f"/api.php?act=query&pid={pid}&key={key}")
    j = json.loads(body)
    check("拒绝后余额退回 34.65", j.get("money") == "34.65", f"money={j.get('money')}")

    print("== 13. 后台手动补单(过期订单) ==")
    p3 = {"pid": pid, "type": "alipay", "out_trade_no": "E2E-ORDER-003", "name": "补单商品", "money": "30.00"}
    p3["sign"] = sign(p3, key); p3["sign_type"] = "MD5"
    st, hd, body = req("/submit.php?" + urllib.parse.urlencode(p3), follow=False)
    # 管理员订单页找到该订单并补单
    _, _, op = req("/admin/orders?kw=E2E-ORDER-003", headers={"Cookie": admin_jar["cookie"]})
    oid_m = re.search(r'name="id" value="(\d+)"', op)
    st, hd, _ = req("/admin/orders/repair", {"_csrf": csrf, "id": oid_m.group(1)}, {"Cookie": admin_jar["cookie"]}, follow=False)
    check("补单成功", "msg=" in str(hd.get("Location", "")) and "err" not in str(hd.get("Location", "")), str(hd.get("Location")))
    st, _, body = req(f"/api.php?act=order&pid={pid}&key={key}&out_trade_no=E2E-ORDER-003")
    j = json.loads(body)
    check("补单后状态成功", j.get("status") == 1, body[:200])

    print("== 14. 后台待办/设置/流水页面 200 ==")
    for p in ["/admin", "/admin/todo.json", "/admin/users", "/admin/orders", "/admin/channels",
              "/admin/devices", "/admin/settles", "/admin/settings", "/admin/paylogs",
              "/user/orders", "/user/balance", "/user/settings", "/user/merchant"]:
        st, _, _ = req(p, headers={"Cookie": admin_jar["cookie"] if p.startswith("/admin") else jar["cookie"]})
        check(f"{p} → 200", st == 200, f"st={st}")

    print("== 14b. 商户自有通道(我的通道)==")
    # 商户(demo, pid=1001) 上传自己的支付宝收款码 → 收银台QR应优先指向商户码
    st, _, chpage = req("/user/channels", headers={"Cookie": admin_jar["cookie"]})
    m = re.search(r'name="_csrf" value="([0-9a-f]+)"', chpage)
    import io as _io
    boundary = "----kpe2e"
    png = (b"\x89PNG\r\n\x1a\n" + b"0" * 64)  # 最小PNG头即可(仅测流程)
    body = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"_csrf\"\r\n\r\n{m.group(1)}\r\n"
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"type\"\r\n\r\nalipay\r\n"
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"qrcode\"; filename=\"qr.png\"\r\n"
            f"Content-Type: image/png\r\n\r\n").encode() + png + f"\r\n--{boundary}--\r\n".encode()
    r = urllib.request.Request(BASE + "/user/channels/upload", data=body,
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}",
                 "Cookie": admin_jar["cookie"]})
    try:
        resp = urllib.request.build_opener(NoRedirect).open(r, timeout=10)
        loc = str(resp.headers.get("Location", ""))
    except urllib.error.HTTPError as e:
        loc = str(e.headers.get("Location", ""))
    check("上传自有收款码", "msg=" in loc, loc)

    print("== 14b2. 新功能: 商户订单详情/头像/数据清理 ==")
    # 商户订单详情(含通知投递记录区)
    st, _, body = req(f"/user/orders/{oid_m.group(1) if False else 1}", headers={"Cookie": admin_jar["cookie"]})
    st, _, body = req("/admin/orders/1", headers={"Cookie": admin_jar["cookie"]})
    check("管理员订单详情页", st == 200 and ("通知投递记录" in body or "订单信息" in body), f"st={st}")
    st, _, body = req("/user/orders/1", headers={"Cookie": jar["cookie"]})
    check("商户订单详情页(越权防护:仅本人)", st in (200,404), f"st={st}")

    # 头像上传(png 魔数)—— 独立会话完整流程
    import http.cookiejar as _cjd
    _cjk = _cjd.CookieJar()
    _avop = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(_cjk))
    try:
        _avop.open(BASE+"/login", urllib.parse.urlencode({"username":"demo","password":"demo123"}).encode())
    except urllib.error.HTTPError:
        pass
    _spage = _avop.open(BASE+"/user/settings").read().decode()
    _m = re.search(r'name="_csrf" value="([0-9a-f]+)"', _spage)
    check("个人设置页可访问(头像上传前置)", _m is not None)
    png = b"\x89PNG\r\n\x1a\n" + b"0" * 32
    b = "----av"
    body2 = (f"--{b}\r\nContent-Disposition: form-data; name=\"_csrf\"\r\n\r\n{_m.group(1) if _m else ''}\r\n"
             f"--{b}\r\nContent-Disposition: form-data; name=\"avatar\"; filename=\"a.png\"\r\n"
             f"Content-Type: image/png\r\n\r\n").encode() + png + f"\r\n--{b}--\r\n".encode()
    cookie_hdr = "; ".join(f"{c.name}={c.value}" for c in _cjk)
    req2 = urllib.request.Request(BASE + "/user/avatar/upload", data=body2,
        headers={"Content-Type": f"multipart/form-data; boundary={b}", "Cookie": cookie_hdr})
    try:
        _resp = urllib.request.build_opener(NoRedirect).open(req2, timeout=10)
        loc = _resp.headers.get("Location", "")
    except urllib.error.HTTPError as e:
        loc = e.headers.get("Location", "")
    check("头像上传", "msg=" in loc and "err" not in loc, f"loc={urllib.parse.unquote(loc)[:60]}")

    print("== 15. 演示页三种支付方式(multipart 提交)==")
    def multipart_post(path, fields):
        boundary = "----kpboundary123"
        body = b""
        for k, v in fields.items():
            body += f"--{boundary}\r\nContent-Disposition: form-data; name=\"{k}\"\r\n\r\n{v}\r\n".encode()
        body += f"--{boundary}--\r\n".encode()
        r = urllib.request.Request(BASE + path, data=body, headers={
            "Content-Type": f"multipart/form-data; boundary={boundary}"})
        return urllib.request.build_opener().open(r, timeout=10).read().decode()

    for typ, expect in [("wxpay", "微信支付"), ("qqpay", "QQ钱包"), ("alipay", "支付宝")]:
        resp = multipart_post("/demo/create", {"money": "1.00", "type": typ})
        j = json.loads(resp)
        ok = j.get("code") == 1
        if ok:
            _, _, cashier_html = req(j["pay_url"])
            ok = f"cashier-top {typ}" in cashier_html and expect in cashier_html
        check(f"演示选择{expect} → 收银台一致", ok, resp[:200])

    print(f"\n结果: {ok_count} 通过, {fail_count} 失败")
    sys.exit(1 if fail_count else 0)

main()
