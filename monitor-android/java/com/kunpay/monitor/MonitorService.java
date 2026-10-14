package com.kunpay.monitor;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.Intent;
import android.content.SharedPreferences;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.IBinder;
import android.service.notification.NotificationListenerService;
import android.service.notification.StatusBarNotification;

import java.text.SimpleDateFormat;
import java.util.Arrays;
import java.util.Date;
import java.util.HashSet;
import java.util.Locale;
import java.util.Set;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * 收款通知监听服务:
 * 监听微信/支付宝/QQ 的收款通知,提取金额后按 /monitor/push 协议上报平台。
 * 兼容 V免签(vmq)监控APP的使用方式 —— 授权通知使用权并保持后台运行即可。
 */
public class MonitorService extends NotificationListenerService {

    private static final String CH_ID = "kunpay_monitor";
    private static final String PREFS = "kunpay";
    private static final String ACTION_LOG = "com.kunpay.LOG";
    /** 微信 / 支付宝 / QQ 包名 */
    private static final Set<String> KNOWN_PACKAGES = new HashSet<>(Arrays.asList(
            "com.tencent.mm",                    // 微信
            "com.eg.android.AlipayGphone",       // 支付宝
            "com.tencent.mobileqq",              // QQ
            "com.tencent.tim"                    // TIM(QQ通道)
    ));
    /** 金额提取: 关键词(收款/到账/入账/入帐)后最近的数字 */
    private static final Pattern AMOUNT = Pattern.compile(
            "(?:收款|到账|入账|入帐|转账)[^0-9]{0,12}(([0-9]{1,3}(,[0-9]{3})+|[0-9]+)(?:\\.[0-9]{1,2})?)");

    private final HandlerThread worker = new HandlerThread("kunpay-worker");
    private Handler handler;
    private Runnable heartbeat;
    private SharedPreferences prefs;
    private int hbOkCount = 0;

    @Override
    public IBinder onBind(Intent intent) {
        return super.onBind(intent);
    }

    @Override
    public void onCreate() {
        super.onCreate();
        prefs = getSharedPreferences(PREFS, MODE_PRIVATE);
        worker.start();
        handler = new Handler(worker.getLooper());
        startOwnForeground();
        startHeartbeat();
        log("监控服务已启动,等待收款通知...");
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        if (handler != null) handler.removeCallbacksAndMessages(null);
        super.onDestroy();
    }

    /** 常驻前台通知,降低被系统回收概率 */
    private void startOwnForeground() {
        NotificationManager nm = getSystemService(NotificationManager.class);
        NotificationChannel ch = new NotificationChannel(CH_ID, "收款监控",
                NotificationManager.IMPORTANCE_LOW);
        ch.setDescription("保持监控服务常驻运行");
        nm.createNotificationChannel(ch);
        Notification n = new Notification.Builder(this, CH_ID)
                .setContentTitle("KunPay 监控运行中")
                .setContentText("正在监听微信/支付宝/QQ收款通知")
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setOngoing(true)
                .build();
        try {
            startForeground(1001, n);
        } catch (Exception e) {
            log("前台服务启动失败: " + e.getMessage());
        }
    }

    /** 每 30 秒向平台发送一次心跳 */
    private void startHeartbeat() {
        heartbeat = new Runnable() {
            @Override
            public void run() {
                final String server = prefs.getString("server", "");
                final String token = prefs.getString("token", "");
                if (!server.isEmpty() && !token.isEmpty()) {
                    Api.heartbeat(server, token, new Api.Callback() {
                        @Override
                        public void onResult(boolean ok, String msg) {
                            if (ok) {
                                hbOkCount++;
                                prefs.edit().putString("last_hb",
                                        new SimpleDateFormat("MM-dd HH:mm:ss", Locale.CHINA)
                                                .format(new Date())).apply();
                                if (hbOkCount == 1) log("✓ 平台连接成功,设备已上线");
                            } else {
                                log("✗ 心跳失败: " + msg);
                            }
                        }
                    });
                }
                handler.postDelayed(this, 30_000L);
            }
        };
        handler.post(heartbeat);
    }

    /** 通知到达:过滤收款类通知并提取金额上报 */
    @Override
    public void onNotificationPosted(StatusBarNotification sbn) {
        try {
            String pkg = sbn.getPackageName();
            CharSequence titleCs = sbn.getNotification().extras.getCharSequence(Notification.EXTRA_TITLE);
            CharSequence textCs = sbn.getNotification().extras.getCharSequence(Notification.EXTRA_TEXT);
            CharSequence bigCs = sbn.getNotification().extras.getCharSequence(Notification.EXTRA_BIG_TEXT);
            String title = titleCs == null ? "" : titleCs.toString();
            String text = (textCs == null ? "" : textCs.toString());
            if (bigCs != null && bigCs.length() > text.length()) text = bigCs.toString();
            String content = (title + " " + text).trim();
            if (content.isEmpty()) return;
            boolean own = getPackageName().equals(pkg);
            if (own) return; // 忽略自己的常驻通知

            boolean known = KNOWN_PACKAGES.contains(pkg);
            boolean strict = prefs.getBoolean("strict_source", true); // 严格来源模式: 防聊天文本伪造到账
            if (strict && !known) {
                log("[" + shortPkg(pkg) + "] 非官方应用通知已忽略(严格来源模式)");
                return;
            }
            boolean keywordHit = content.contains("收款") || content.contains("到账")
                    || content.contains("入账") || content.contains("入帐");
            if (!known && !keywordHit) return;

            Matcher m = AMOUNT.matcher(content);
            if (!m.find()) {
                if (known) log("[" + shortPkg(pkg) + "] 检测到通知但未识别到金额: " + clip(content));
                return;
            }
            String money = m.group(1).replace(",", ""); // 千分位(1,234.50 → 1234.50)
            String type = mapType(pkg, content);

            boolean enabled = prefs.getBoolean("ch_" + type, true);
            if (!enabled) {
                log("[" + type + "] 通道已关闭,忽略 ¥" + money);
                return;
            }
            final String fContent = clip(content);
            final String fMoney = money, fType = type;
            log("[" + shortPkg(pkg) + "] " + content + " → 上报 " + type + " ¥" + money);
            Api.push(prefs.getString("server", ""), prefs.getString("token", ""),
                    fType, fMoney, fContent, new Api.Callback() {
                        @Override
                        public void onResult(boolean ok, String msg) {
                            if (ok) {
                                log("✓ 上报成功: " + msg);
                            } else {
                                log("✗ 上报失败: " + msg + " (已加入重试队列)");
                                enqueuePending(fMoney, fType, fContent);
                                handler.postDelayed(new Runnable() { @Override public void run() {
                                    flushPending();
                                }}, 30_000L);
                            }
                        }
                    });
        } catch (Exception e) {
            log("处理通知异常: " + e.getMessage());
        }
    }

    private String mapType(String pkg, String content) {
        if ("com.tencent.mm".equals(pkg)) return "wxpay";
        if ("com.eg.android.AlipayGphone".equals(pkg)) return "alipay";
        if ("com.tencent.mobileqq".equals(pkg) || "com.tencent.tim".equals(pkg)) return "qqpay";
        // 非白名单包(测试/聚合通知): 按文案推断
        if (content.contains("微信")) return "wxpay";
        if (content.contains("支付宝")) return "alipay";
        if (content.contains("QQ")) return "qqpay";
        return "alipay";
    }

    private String shortPkg(String pkg) {
        if ("com.tencent.mm".equals(pkg)) return "微信";
        if ("com.eg.android.AlipayGphone".equals(pkg)) return "支付宝";
        if ("com.tencent.mobileqq".equals(pkg) || "com.tencent.tim".equals(pkg)) return "QQ";
        return pkg;
    }

    /** 追加一条日志并通过广播刷新界面 */
    // ---- 上报失败重试队列(持久化到 prefs,心跳/定时触发冲刷) ----
    private static final int PENDING_MAX = 50;

    private void enqueuePending(String money, String type, String content) {
        try {
            org.json.JSONArray arr = new org.json.JSONArray(prefs.getString("pending_pushes", "[]"));
            if (arr.length() >= PENDING_MAX) return; // 防无限堆积
            org.json.JSONObject o = new org.json.JSONObject();
            o.put("money", money);
            o.put("type", type);
            o.put("content", content);
            o.put("ts", System.currentTimeMillis());
            arr.put(o);
            prefs.edit().putString("pending_pushes", arr.toString()).apply();
        } catch (Exception ignore) {}
    }

    private void flushPending() {
        try {
            org.json.JSONArray arr = new org.json.JSONArray(prefs.getString("pending_pushes", "[]"));
            if (arr.length() == 0) return;
            org.json.JSONArray remain = new org.json.JSONArray();
            String server = prefs.getString("server", "");
            String token = prefs.getString("token", "");
            if (server.isEmpty() || token.isEmpty()) return;
            for (int i = 0; i < arr.length(); i++) {
                final org.json.JSONObject o = arr.getJSONObject(i);
                final java.util.concurrent.CountDownLatch latch =
                        new java.util.concurrent.CountDownLatch(1);
                final boolean[] done = {false};
                Api.push(server, token, o.optString("type"), o.optString("money"),
                        o.optString("content"), new Api.Callback() {
                            @Override public void onResult(boolean ok, String msg) {
                                done[0] = ok;
                                latch.countDown();
                            }
                        });
                latch.await(10, java.util.concurrent.TimeUnit.SECONDS);
                if (!done[0]) remain.put(o);
            }
            prefs.edit().putString("pending_pushes", remain.toString()).apply();
            if (remain.length() > 0) log("重试队列仍有 " + remain.length() + " 笔待上报");
            else log("重试队列已全部上报完成");
        } catch (Exception e) {
            log("重试队列异常: " + e.getMessage());
        }
    }

    private void log(String line) {
        String ts = new SimpleDateFormat("HH:mm:ss", Locale.CHINA).format(new Date());
        String entry = ts + "  " + line;
        String old = prefs.getString("logs", "");
        String merged = entry + "\n" + old;
        String[] lines = merged.split("\n", 81);
        if (lines.length > 60) {
            StringBuilder sb = new StringBuilder();
            for (int i = 0; i < 60; i++) sb.append(lines[i]).append("\n");
            merged = sb.toString();
        }
        prefs.edit().putString("logs", merged.trim()).apply();
        Intent i = new Intent(ACTION_LOG);
        i.setPackage(getPackageName());
        sendBroadcast(i);
    }

    private String clip(String s) {
        return s.length() > 60 ? s.substring(0, 60) + "…" : s;
    }
}
