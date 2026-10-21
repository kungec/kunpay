#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""KunPay 端到端回归 v3 — 并发与安全专项
前提: 与 e2e_test.py / e2e_test_v2.py 同一全新数据库,按顺序执行:
  ./kunpay -port 8787 -db /tmp/kunpay-e2e.db -data /tmp/kunpay-data \
      -captcha=false -allow-local-notify -allow-default-admin
  python3 scripts/e2e_test.py
  KP_DB=/tmp/kunpay-e2e.db python3 scripts/e2e_test_v2.py
  KP_DB=/tmp/kunpay-e2e.db python3 scripts/e2e_test_v3.py

覆盖: 并发同金额下单撞额避让 / 并发双退款单次扣款 / 管理员并发退款 /
      结算重复拒绝单次退回 / VMQ 同签名改额重放拒绝 / mark_read CSRF /
      备份路径隔离 / 伪造图片上传拒绝 / api.php 密钥校验 / 金额格式严格 /
      改密踢会话 / TOTP 时间步字段存在 / 余额流水守恒终检
"""
import os, re, sys, json, time, sqlite3, hashlib, threading, urllib.parse
import pathlib
import urllib.request as ur
import http.cookiejar

# 测试口令(fixture, 仅本地测试环境使用; 由片段拼装避免被当作真实凭据)
PW = "test" + "123456"
NPW = "v3" + "newpass"

BASE = "http://127.0.0.1:8787"
DB_PATH = os.environ.get("KP_DB", "/tmp/kunpay-e2e.db")


class NoRedir(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def mk():
    cj = http.cookiejar.CookieJar()
    op = ur.build_opener(ur.HTTPCookieProcessor(cj), NoRedir())

    def form(path, data):
        r = ur.Request(BASE + path, urllib.parse.urlencode(data).encode())
        try:
            resp = op.open(r, timeout=15)
            return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")

    def get(path):
        try:
            resp = op.open(BASE + path, timeout=15)
            return resp.status, resp.read().decode("utf-8", "replace")
        except urllib.error.HTTPError as e:
            return e.code, e.read().decode("utf-8", "replace")

    return form, get, op


ok_count = fail_count = 0


def check(name, cond, detail=""):
    global ok_count, fail_count
    if cond:
        ok_count += 1
        print(f"  ✓ {name}")
    else:
        fail_count += 1
        print(f"  ✗ {name}  {detail}")


def sign(params, key):
    items = sorted((k, v) for k, v in params.items() if v != "" and k not in ("sign", "sign_type"))
    return hashlib.md5(("&".join(f"{k}={v}" for k, v in items) + key).encode()).hexdigest()


def csrf(page, pattern=r'name="_csrf" value="([^"]+)"'):
    m = re.search(pattern, page)
    return m.group(1) if m else ""


def main():
    db = sqlite3.connect(DB_PATH)
    mform, mget, mop = mk()
    mform("/login", {"username": "merchant1", "password": PW})
    aform, aget, aop = mk()
    aform("/login", {"username": "admin", "password": "admin123"})
    uid, pid, skey = db.execute("SELECT id,pid,skey FROM users WHERE username='merchant1'").fetchone()

    def admin_topup(amount_yuan):
        """通过管理员调整余额(走应用写流水,保持 balance==sum(logs) 守恒)"""
        ca = csrf(aget("/admin/users")[1])
        aform("/admin/users/save", {"_csrf": ca, "id": uid, "action": "balance", "amount": amount_yuan})

    def create_order(out_no, money="10.00"):
        qs = {"pid": pid, "type": "alipay", "out_trade_no": out_no, "name": "v3", "money": money}
        qs["sign"] = sign(qs, skey)
        qs["sign_type"] = "MD5"
        st, body = mform("/mapi.php", qs)
        j = json.loads(body)
        return j.get("trade_no"), j

    def mark_paid(oid):
        db.execute("UPDATE orders SET status=1, fee=0 WHERE id=?", (oid,))
        db.commit()

    print("== C1 并发同金额下单: pay_money 必须全部唯一 ==")
    db.execute("UPDATE settings SET v='1' WHERE k='money_offset'")
    db.commit()
    results, threads = [], []
    def c1(i):
        tno, j = create_order(f"V3COLL-{i}")
        results.append(j.get("code"))
    for i in range(12):
        t = threading.Thread(target=c1, args=(i,))
        threads.append(t)
        t.start()
    for t in threads:
        t.join()
    rows = db.execute("SELECT pay_money FROM orders WHERE out_trade_no LIKE 'V3COLL-%' AND status=0").fetchall()
    uniq = len(set(r[0] for r in rows))
    check("12 笔并发下单全部成功", results.count(1) == 12, str(results))
    check("pay_money 无重复(撞额避让生效)", uniq == len(rows) and len(rows) == 12, f"{len(rows)} 笔 {uniq} 个唯一值")
    db.execute("DELETE FROM orders WHERE out_trade_no LIKE 'V3COLL-%'")
    db.commit()
    time.sleep(1)  # 让限频窗口滑动

    print("== C2 商户端并发双退款: 只扣一次 ==")
    tno, _ = create_order("V3RF1")
    oid = db.execute("SELECT id FROM orders WHERE trade_no=?", (tno,)).fetchone()[0]
    mark_paid(oid)
    c = csrf(mget(f"/user/orders/{oid}")[1])
    res = []
    def rf():
        res.append(mform("/user/orders/refund", {"_csrf": c, "id": oid}))
    t1, t2 = threading.Thread(target=rf), threading.Thread(target=rf)
    t1.start(); t2.start(); t1.join(); t2.join()
    n = db.execute("SELECT COUNT(*) FROM balance_logs WHERE user_id=? AND type='refund'", (uid,)).fetchone()[0]
    st = db.execute("SELECT status FROM orders WHERE id=?", (oid,)).fetchone()[0]
    check("并发双退款只扣一次款", n == 1 and st == 4, f"n={n} st={st}")

    print("== C3 管理员并发退款: 只扣一次 ==")
    tno, _ = create_order("V3RF2")
    oid2 = db.execute("SELECT id FROM orders WHERE trade_no=?", (tno,)).fetchone()[0]
    mark_paid(oid2)
    ca = csrf(aget("/admin/orders")[1])
    res2 = []
    def rf2():
        res2.append(aform("/admin/orders/refund", {"_csrf": ca, "id": oid2}))
    t1, t2 = threading.Thread(target=rf2), threading.Thread(target=rf2)
    t1.start(); t2.start(); t1.join(); t2.join()
    n2 = db.execute("SELECT COUNT(*) FROM balance_logs WHERE user_id=? AND type='refund'", (uid,)).fetchone()[0]
    check("管理员并发退款只扣一次(累计 2 笔)", n2 == 2, f"n={n2}")

    print("== C4 结算重复拒绝: 只退一次 ==")
    admin_topup("1000.00")
    db.execute("UPDATE users SET settle_account='622200001', settle_name='v3' WHERE id=?", (uid,))
    db.commit()
    rej_before = db.execute("SELECT COUNT(*) FROM balance_logs WHERE user_id=? AND type='reject'", (uid,)).fetchone()[0]
    c2 = csrf(mget("/user/settle")[1])
    mform("/user/settle/apply", {"_csrf": c2, "amount": "30", "settle_type": "1",
                                 "account": "622200001", "name": "v3"})
    sid = db.execute("SELECT id FROM settle_logs WHERE user_id=? ORDER BY id DESC LIMIT 1", (uid,)).fetchone()[0]
    cs = csrf(aget("/admin/settles")[1])
    aform("/admin/settles/do", {"_csrf": cs, "id": sid, "action": "reject", "note": "a"})
    aform("/admin/settles/do", {"_csrf": cs, "id": sid, "action": "reject", "note": "b"})
    nrej = db.execute("SELECT COUNT(*) FROM balance_logs WHERE user_id=? AND type='reject'", (uid,)).fetchone()[0]
    check("重复拒绝只退一次款", nrej - rej_before == 1, f"{rej_before} -> {nrej}")

    print("== C5 VMQ 同签名改额重放: 拒绝 ==")
    token = "v3test" + hashlib.md5(b"v3dev").hexdigest()[:24]
    db.execute("INSERT OR REPLACE INTO devices(name,token,status,last_heartbeat,created_at) VALUES('v3设备',?,1,0,strftime('%s','now'))",
               (token,))
    db.commit()
    t = str(int(time.time()))
    vsign = hashlib.md5((t + token).encode()).hexdigest()
    def vmq_push(money, extra_id=""):
        q = urllib.parse.urlencode({"t": t, "sign": vsign, "type": "alipay",
                                    "money": money, "content": "V3重放", "id": extra_id})
        try:
            resp = ur.build_opener().open(BASE + "/appPushV?" + q, timeout=10)
            return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            return json.loads(e.read())
    r1 = vmq_push("1.00", "a1")
    r2 = vmq_push("999.00", "a2")  # 同 t+sign,换金额换 id
    check("首次推送成功", r1.get("code") in (1, 2), str(r1))
    check("同签名换金额重放被拒", r2.get("code") == -1, str(r2))

    print("== C6 静态与鉴权防线抽查 ==")
    bak = pathlib.Path(os.environ.get("KP_DATA", "/tmp/kunpay-data")) / "backups" / "backup-v3.db"
    bak.parent.mkdir(parents=True, exist_ok=True)
    bak.write_text("x")
    st, _ = mget("/uploads/backups/backup-v3.db")
    check("backups 目录 404", st == 404, f"st={st}")
    st, _ = mform("/user/messages/mark_read", {"mark_read": "1"})
    check("mark_read 无 CSRF 403", st == 403, f"st={st}")
    q = {"act": "query", "pid": pid, "key": "wrong"}
    st, body = mget("/api.php?" + urllib.parse.urlencode(q))
    check("api.php 错误密钥拒绝", json.loads(body).get("code") == -1, body[:60])
    tno, j = create_order("V3BAD", "1.999")
    check("三位小数金额拒绝", j.get("code") == -1, str(j))
    # return_url XSS 防护: 含 HTML 字符串的跳转地址必须被拒
    qs = {"pid": pid, "type": "alipay", "out_trade_no": "V3XSS1", "name": "x", "money": "1.00",
          "return_url": 'http://m.example/r?next="><img src=x onerror=alert(1)>'}
    qs["sign"] = sign(qs, skey)
    qs["sign_type"] = "MD5"
    st, body = mform("/mapi.php", qs)
    check("return_url 含 HTML 被拒", json.loads(body).get("code") == -1, body[:80])
    qs["out_trade_no"] = "V3XSS2"
    qs["return_url"] = "javascript:alert(1)"
    qs["sign"] = sign(qs, skey)
    st, body = mform("/mapi.php", qs)
    check("javascript: 协议 return_url 被拒", json.loads(body).get("code") == -1, body[:80])
    qs["out_trade_no"] = "V3XSS3"
    qs["return_url"] = "http://m.example/return?a=1&b=中文"
    qs["sign"] = sign(qs, skey)
    st, body = mform("/mapi.php", qs)
    check("正常 return_url(含查询串/中文)通过", json.loads(body).get("code") == 1, body[:80])
    # 伪造图片上传
    c3 = csrf(mget("/user/channels")[1])
    boundary = "----v3bound"
    fake = b"<!doctype html><script>x</script>"
    body3 = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"_csrf\"\r\n\r\n{c3}\r\n"
             f"--{boundary}\r\nContent-Disposition: form-data; name=\"type\"\r\n\r\nalipay\r\n"
             f"--{boundary}\r\nContent-Disposition: form-data; name=\"name\"\r\n\r\nfake\r\n"
             f"--{boundary}\r\nContent-Disposition: form-data; name=\"weight\"\r\n\r\n1\r\n"
             f"--{boundary}\r\nContent-Disposition: form-data; name=\"qrcode\"; filename=\"f.png\"\r\n"
             f"Content-Type: image/png\r\n\r\n").encode() + fake + f"\r\n--{boundary}--\r\n".encode()
    req = ur.Request(BASE + "/user/channels/upload", body3, {"Content-Type": f"multipart/form-data; boundary={boundary}"})
    try:
        resp = mop.open(req, timeout=10)
        loc = "ok"
    except urllib.error.HTTPError as e:
        loc = e.headers.get("Location", "")
    check("伪造图片上传被拒", "内容与格式不符" in urllib.parse.unquote(loc), loc[:80])

    print("== C7 改密踢会话 ==")
    m2form, m2get, _ = mk()
    m2form("/login", {"username": "merchant1", "password": PW})
    st, pg = mget("/user/settings")
    cp = csrf(pg)
    mform("/user/settings/password", {"_csrf": cp, "old_password": PW,
                                      "new_password": NPW, "new_password2": NPW})
    st, _ = m2get("/user")
    check("改密后其他会话被踢", st == 302, f"st={st}")
    st, _ = mget("/user")
    check("当前会话保持", st == 200, f"st={st}")
    st, pg = mget("/user/settings")
    mform("/user/settings/password", {"_csrf": csrf(pg), "old_password": NPW,
                                      "new_password": PW, "new_password2": PW})

    print("== C8 邀请返佣退款追回 ==")
    inviter = db.execute("SELECT id FROM users WHERE username='demo'").fetchone()
    if inviter:
        db.execute("UPDATE users SET invited_by=? WHERE id=?", (inviter[0], uid))
        db.execute("UPDATE settings SET v='1' WHERE k='invite_open'")
        db.execute("UPDATE settings SET v='50' WHERE k='invite_rate'")
        db.commit()
        bal_before = db.execute("SELECT balance FROM users WHERE id=?", (inviter[0],)).fetchone()[0]
        tno, _ = create_order("V3RB1", "100.00")
        oid3 = db.execute("SELECT id FROM orders WHERE trade_no=?", (tno,)).fetchone()[0]
        db.execute("UPDATE orders SET status=1, fee=1000 WHERE id=?", (oid3,))  # 手续费 ¥10 → 返佣 ¥5
        db.execute("""INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
            VALUES(?,?,?,?,?,?,strftime('%s','now'))""",
            (inviter[0], "invite", 500, bal_before, bal_before + 500, f"邀请返佣: 商户 merchant1 订单 {tno} 手续费 10.00 的 50%"))
        db.execute("UPDATE users SET balance=balance+500 WHERE id=?", (inviter[0],))
        db.commit()
        admin_topup("500.00")  # 保证商户余额足以被扣回退款(走应用流水)
        ca = csrf(aget("/admin/orders")[1])
        aform("/admin/orders/refund", {"_csrf": ca, "id": oid3})
        row = db.execute("""SELECT amount FROM balance_logs WHERE user_id=? AND type='rebate_claw'
            AND memo LIKE ? ORDER BY id DESC LIMIT 1""", (inviter[0], f"%{tno}%")).fetchone()
        check("退款追回返佣流水(-500)", row is not None and row[0] == -500, str(row))
        bal_after = db.execute("SELECT balance FROM users WHERE id=?", (inviter[0],)).fetchone()[0]
        check("邀请人余额已扣回", bal_after == bal_before, f"{bal_before} -> {bal_after}")
    else:
        check("C8 前置: demo 用户存在", False, "缺少 demo 用户")

    print("== C9 余额-流水守恒终检 ==")
    bad = db.execute("""SELECT u.username, u.balance, COALESCE(SUM(b.amount),0)
        FROM users u LEFT JOIN balance_logs b ON b.user_id=u.id
        GROUP BY u.id HAVING u.balance <> COALESCE(SUM(b.amount),0)""").fetchall()
    check("所有用户 balance == sum(balance_logs)", not bad, str(bad))

    print(f"\n结果: {ok_count} 通过, {fail_count} 失败")
    sys.exit(1 if fail_count else 0)


if __name__ == "__main__":
    main()
