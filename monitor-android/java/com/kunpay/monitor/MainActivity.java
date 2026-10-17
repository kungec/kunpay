package com.kunpay.monitor;

import android.Manifest;
import android.app.Activity;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.SharedPreferences;
import android.content.res.ColorStateList;
import android.graphics.Color;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.provider.Settings;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

/** 主界面: 设备绑定、通道开关、授权入口、实时日志 */
public class MainActivity extends Activity {

    private static final String PREFS = "kunpay";

    private EditText etServer, etToken;
    private TextView tvConn, tvLog, statusText, tvHeart;
    private Button btnListener, btnBattery;
    private View statusDot;
    private Switch swAlipay, swWxpay, swQqpay;
    private SharedPreferences prefs;
    private int hbOkCount = 0;

    private final BroadcastReceiver logReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            refreshLog();
            refreshStatus();
        }
    };

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);
        prefs = getSharedPreferences(PREFS, MODE_PRIVATE);
        wireStrictSwitch();
        wireVoiceSwitch();

        etServer = findViewById(R.id.etServer);
        etToken = findViewById(R.id.etToken);
        tvConn = findViewById(R.id.tvConn);
        tvLog = findViewById(R.id.tvLog);
        statusText = findViewById(R.id.statusText);
        statusDot = findViewById(R.id.statusDot);
        tvHeart = findViewById(R.id.tvHeart);
        swAlipay = findViewById(R.id.swAlipay);
        swWxpay = findViewById(R.id.swWxpay);
        swQqpay = findViewById(R.id.swQqpay);
        Button btnSave = findViewById(R.id.btnSave);
        Button btnTest = findViewById(R.id.btnTest);
        btnListener = findViewById(R.id.btnListener);
        btnBattery = findViewById(R.id.btnBattery);
        View btnClear = findViewById(R.id.btnClear);

        // 恢复配置
        etServer.setText(prefs.getString("server", ""));
        etToken.setText(prefs.getString("token", ""));
        swAlipay.setChecked(prefs.getBoolean("ch_alipay", true));
        swWxpay.setChecked(prefs.getBoolean("ch_wxpay", true));
        swQqpay.setChecked(prefs.getBoolean("ch_qqpay", true));
        refreshLog();
        refreshStatus();

        // 扫码配对: 处理 kunpay://pair?server=xx&token=xx deep link(冷启动)
        handlePairing(getIntent());

        // 安全: 不再接受外部 Intent extras 配置(任意应用可借此重配 Token 外传);
        // 初始化一律通过本界面手动填写或扫码配对。如需批量配置,请用设备管理方案。

        btnSave.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                connectAndStart();
            }
        });

        btnTest.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                String server = prefs.getString("server", "");
                String token = prefs.getString("token", "");
                if (server.isEmpty() || token.isEmpty()) {
                    toast("请先保存设备绑定");
                    return;
                }
                toast("已发送测试上报 ¥0.01");
                Api.push(server, token, "alipay", "0.01", "测试上报", new Api.Callback() {
                    @Override
                    public void onResult(final boolean ok, final String msg) {
                        runOnUiThread(new Runnable() {
                            @Override
                            public void run() {
                                tvConn.setText((ok ? "✓ 测试上报: " : "✗ 测试失败: ") + msg);
                                tvConn.setTextColor(getColor(ok ? R.color.success : R.color.danger));
                            }
                        });
                    }
                });
            }
        });

        btnListener.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                boolean granted = checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)
                        == android.content.pm.PackageManager.PERMISSION_GRANTED;
                if (listenerEnabled() && !granted && android.os.Build.VERSION.SDK_INT >= 33) {
                    ensureNotificationPermission();
                    return;
                }
                try {
                    startActivity(new Intent(Settings.ACTION_NOTIFICATION_LISTENER_SETTINGS));
                } catch (Exception e) {
                    toast("无法打开设置页");
                }
            }
        });

        btnBattery.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                try {
                    startActivity(new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                            Uri.parse("package:" + getPackageName())));
                } catch (Exception e) {
                    toast("当前系统不支持该设置,请手动在系统设置中关闭电池优化");
                }
            }
        });

        View.OnClickListener saveSwitch = new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                prefs.edit()
                        .putBoolean("ch_alipay", swAlipay.isChecked())
                        .putBoolean("ch_wxpay", swWxpay.isChecked())
                        .putBoolean("ch_qqpay", swQqpay.isChecked()).apply();
            }
        };
        swAlipay.setOnClickListener(saveSwitch);
        swWxpay.setOnClickListener(saveSwitch);
        swQqpay.setOnClickListener(saveSwitch);

        btnClear.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
                prefs.edit().putString("logs", "").apply();
                refreshLog();
            }
        });
    }

    private void updatePermWarning() {
        boolean enabled = listenerEnabled();
        android.view.View warning = findViewById(R.id.permWarning);
        if (warning == null) return;
        warning.setVisibility(enabled ? android.view.View.GONE : android.view.View.VISIBLE);
        android.view.View btn = warning.findViewById(R.id.btnGoPerm);
        if (btn != null) {
            btn.setOnClickListener(new View.OnClickListener() {
                @Override
                public void onClick(View v) {
                    try {
                        startActivity(new android.content.Intent(Settings.ACTION_NOTIFICATION_LISTENER_SETTINGS));
                    } catch (Exception ignore) {}
                }
            });
        }
    }

    public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode == 100) {
            refreshListenerState();
        updatePermWarning();
            ensureService(); // 授权后重连,让系统恢复监听事件投递
            toast("通知权限已更新");
        }
    }

    private final android.os.Handler uiTick = new android.os.Handler();

    private final Runnable uiRefresher = new Runnable() {
        @Override public void run() {
            refreshStatus();      // 兜底刷新: 监听断开/重连与心跳状态实时可见
            ensureService();      // 门禁复查: 授权补齐后自动启动监控(内部幂等)
            uiTick.postDelayed(this, 5000L);
        }
    };

    @Override
    protected void onResume() {
        super.onResume();
        IntentFilter f = new IntentFilter("com.kunpay.LOG");
        if (Build.VERSION.SDK_INT >= 33) {
            registerReceiver(logReceiver, f, Context.RECEIVER_NOT_EXPORTED);
        } else {
            registerReceiver(logReceiver, f);
        }
        refreshLog();
        refreshStatus();
        refreshListenerState();
        ensureService();
        checkAppUpdate();
        uiTick.postDelayed(uiRefresher, 5000L);
        if (android.os.Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)
                    != android.content.pm.PackageManager.PERMISSION_GRANTED) {
            ensureNotificationPermission();
        }
    }

    @Override
    protected void onPause() {
        super.onPause();
        uiTick.removeCallbacks(uiRefresher);
        try {
            unregisterReceiver(logReceiver);
        } catch (Exception ignored) {
        }
    }

    /** 扫码配对(kunpay://pair?server=xx&token=xx): 自动填入并直接连接,
     *  授权齐全时监控自动启动; 缺授权则由门禁黄条点名引导 */
    private void handlePairing(Intent intent) {
        if (intent == null) return;
        android.net.Uri data = intent.getData();
        if (data == null || !"kunpay".equals(data.getScheme()) || !"pair".equals(data.getHost())) return;
        String sv = data.getQueryParameter("server");
        String tk = data.getQueryParameter("token");
        if (sv == null || tk == null || sv.isEmpty() || tk.isEmpty()) return;
        sv = sv.trim().replaceAll("/+$", "");
        tk = tk.trim();
        etServer.setText(sv);
        etToken.setText(tk);
        prefs.edit().putString("server", sv).putString("token", tk).apply();
        toast("✓ 配对成功,正在连接平台...");
        connectAndStart();
    }

    /** 保存配置并连接平台; 授权齐全时自动拉起监控服务(门禁内) */
    private void connectAndStart() {
        String server = etServer.getText().toString().trim().replaceAll("/+$", "");
        String token = etToken.getText().toString().trim();
        if (server.isEmpty() || token.isEmpty()) {
            toast("请填写平台地址与设备Token");
            return;
        }
        prefs.edit().putString("server", server).putString("token", token).apply();
        tvConn.setText("正在连接平台...");
        Api.heartbeat(server, token, new Api.Callback() {
            @Override
            public void onResult(final boolean ok, final String msg) {
                runOnUiThread(new Runnable() {
                    @Override
                    public void run() {
                        if (ok) {
                            prefs.edit().putString("last_hb", now()).apply();
                            tvConn.setText("✓ 平台连接成功,设备已上线 (" + now() + ")");
                            tvConn.setTextColor(getColor(R.color.success));
                            toast(allAuthReady() ? "连接成功,监控已启动"
                                                 : "连接成功,完成授权后监控自动启动");
                            ensureService();
                        } else {
                            tvConn.setText("✗ 连接失败: " + msg);
                            tvConn.setTextColor(getColor(R.color.danger));
                            toast("连接失败");
                        }
                        refreshStatus();
                    }
                });
            }
        });
    }

    /** APP 已在前台时再次收到配对链接(singleTop) */
    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        handlePairing(intent);
        refreshStatus();
    }

    private boolean notifPermOk() {
        if (android.os.Build.VERSION.SDK_INT < 33) return true;
        return checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)
                == android.content.pm.PackageManager.PERMISSION_GRANTED;
    }

    private boolean batteryOk() {
        android.os.PowerManager pm = (android.os.PowerManager) getSystemService(POWER_SERVICE);
        return pm != null && pm.isIgnoringBatteryOptimizations(getPackageName());
    }

    /** 启动门禁: 三项授权(通知使用权/允许通知/电池白名单)全部就绪才允许启动监控 */
    private boolean allAuthReady() {
        return listenerEnabled() && notifPermOk() && batteryOk();
    }

    private String missingAuth() {
        java.util.List<String> miss = new java.util.ArrayList<>();
        if (!listenerEnabled()) miss.add("①通知使用权");
        if (!notifPermOk()) miss.add("②允许通知");
        if (!batteryOk()) miss.add("③电池白名单");
        return String.join("、", miss);
    }

    private boolean serviceStarted = false;

    /** 门禁启动: 授权齐全才拉起监控服务; 缺项在「必要授权」卡黄条提示缺哪项 */
    private void ensureService() {
        updateAuthGate();
        if (!allAuthReady()) return;
        if (serviceStarted) return;
        try {
            startService(new Intent(this, MonitorService.class));
            serviceStarted = true;
        } catch (Exception ignored) {
        }
    }

    private void updateAuthGate() {
        android.widget.TextView gate = findViewById(R.id.authGate);
        if (gate == null) return;
        String miss = missingAuth();
        if (miss.isEmpty()) {
            gate.setVisibility(android.view.View.GONE);
        } else {
            gate.setText("⚠ 监控未启动 —— 还差: " + miss + "\n点上方对应按钮授权,完成后自动开始监控");
            gate.setVisibility(android.view.View.VISIBLE);
        }
        // 电池白名单按钮状态化(与通知使用权按钮一致)
        if (batteryOk()) {
            btnBattery.setText("✓ 电池白名单");
            btnBattery.setBackgroundTintList(ColorStateList.valueOf(Color.parseColor("#E8F7F0")));
            btnBattery.setTextColor(getColor(R.color.success));
        } else {
            btnBattery.setText("③ 电池白名单");
            btnBattery.setBackgroundTintList(null);
            btnBattery.setTextColor(Color.WHITE);
        }
    }

    private void refreshLog() {
        String logs = prefs.getString("logs", "");
        tvLog.setText(logs.isEmpty() ? "等待事件..." : logs);
    }

    private void toast(String msg) {
        Toast.makeText(this, msg, Toast.LENGTH_SHORT).show();
    }

    private String now() {
        return new java.text.SimpleDateFormat("HH:mm:ss", java.util.Locale.CHINA)
                .format(new java.util.Date());
    }

    private boolean listenerEnabled() {
        String listeners = Settings.Secure.getString(getContentResolver(), "enabled_notification_listeners");
        return listeners != null && listeners.contains(getPackageName());
    }

    private android.widget.Switch swStrict;
    private android.widget.Switch swVoice;

    private void wireVoiceSwitch() {
        swVoice = findViewById(R.id.swVoice);
        swVoice.setChecked(prefs.getBoolean("voice_announce", true));
        swVoice.setOnCheckedChangeListener(new android.widget.CompoundButton.OnCheckedChangeListener() {
            @Override
            public void onCheckedChanged(android.widget.CompoundButton b, boolean checked) {
                prefs.edit().putBoolean("voice_announce", checked).apply();
            }
        });
    }

    private void wireStrictSwitch() {
        swStrict = findViewById(R.id.swStrict);
        swStrict.setChecked(prefs.getBoolean("strict_source", true));
        swStrict.setOnCheckedChangeListener(new android.widget.CompoundButton.OnCheckedChangeListener() {
            @Override
            public void onCheckedChanged(android.widget.CompoundButton b, boolean checked) {
                prefs.edit().putBoolean("strict_source", checked).apply();
                toast(checked ? "严格来源模式已开启,仅监听微信/支付宝/QQ官方应用通知"
                              : "严格来源模式已关闭(宽松模式,按关键词识别)");
            }
        });
    }

    private void refreshListenerState() {
        Button b = findViewById(R.id.btnListener);
        boolean granted = checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)
                == android.content.pm.PackageManager.PERMISSION_GRANTED;
        if (listenerEnabled() && granted) {
            b.setText("✓ 已授权通知使用权");
            b.setBackgroundTintList(ColorStateList.valueOf(Color.parseColor("#E8F7F0")));
            b.setTextColor(getColor(R.color.success));
        } else if (listenerEnabled()) {
            // Android 13+: 监听权限已授但缺通知弹窗权限时,监听事件会被系统停发
            b.setText("② 允许通知(否则收不到)");
            b.setBackgroundTintList(ColorStateList.valueOf(Color.parseColor("#FFF3E0")));
            b.setTextColor(Color.parseColor("#B26A00"));
        } else {
            b.setText("① 通知使用权");
            b.setBackgroundTintList(null);
            b.setTextColor(Color.WHITE);
        }
    }

    /** 检查 APP 更新: 每 24h 拉取一次 GitHub Releases,发现新版弹窗引导下载 */
    private void checkAppUpdate() {
        long last = prefs.getLong("update_check_at", 0);
        if (System.currentTimeMillis() - last < 24 * 3600_000L) return;
        new Thread(new Runnable() { @Override public void run() {
            String tag = "", notes = "", url = "";
            try {
                java.net.URL u = new java.net.URL(
                    "https://api.github.com/repos/kungec/kunpay/releases/latest");
                java.net.HttpURLConnection conn = (java.net.HttpURLConnection) u.openConnection();
                conn.setConnectTimeout(8000);
                conn.setReadTimeout(8000);
                conn.setRequestProperty("Accept", "application/vnd.github+json");
                java.io.BufferedReader br = new java.io.BufferedReader(
                        new java.io.InputStreamReader(conn.getInputStream()));
                StringBuilder sb = new StringBuilder();
                String line;
                while ((line = br.readLine()) != null) sb.append(line);
                br.close();
                org.json.JSONObject root = new org.json.JSONObject(sb.toString());
                tag = root.optString("tag_name");
                notes = root.optString("body");
                url = root.optString("html_url");
                if (tag.isEmpty()) throw new Exception("empty");
            } catch (Exception e) {
                return; // 网络不可达/限流,静默跳过
            }
            prefs.edit().putLong("update_check_at", System.currentTimeMillis()).apply();
            if (!isNewerVersion(tag, versionName())) return;
            final String fTag = tag, fNotes = notes, fUrl = url;
            runOnUiThread(new Runnable() { @Override public void run() {
                String shortNotes = fNotes.length() > 400 ? fNotes.substring(0, 400) + "…" : fNotes;
                new android.app.AlertDialog.Builder(MainActivity.this)
                        .setTitle("发现新版本 " + fTag)
                        .setMessage(shortNotes + "\n\n是否前往下载更新?")
                        .setPositiveButton("下载更新", new android.content.DialogInterface.OnClickListener() {
                            @Override public void onClick(android.content.DialogInterface d, int w) {
                                try {
                                    startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(fUrl)));
                                } catch (Exception e) {
                                    toast("无法打开下载页");
                                }
                            }
                        })
                        .setNegativeButton("稍后", null)
                        .show();
            }});
        }}).start();
    }

    private String versionName() {
        try {
            return getPackageManager().getPackageInfo(getPackageName(), 0).versionName;
        } catch (Exception e) {
            return "0.0.0";
        }
    }

    private static int[] splitVer(String v) {
        v = v == null ? "" : v.trim();
        if (v.startsWith("v") || v.startsWith("V")) v = v.substring(1);
        int[] out = {0, 0, 0};
        String[] parts = v.split("\\.", 3);
        if (parts.length < 2) return new int[]{-1, -1, -1};
        for (int i = 0; i < Math.min(3, parts.length); i++) {
            int n = 0;
            for (char ch : parts[i].toCharArray()) {
                if (ch < '0' || ch > '9') break;
                n = n * 10 + (ch - '0');
            }
            out[i] = n;
        }
        return out;
    }

    private static boolean isNewerVersion(String latest, String current) {
        int[] a = splitVer(latest), b = splitVer(current);
        for (int i = 0; i < 3; i++) {
            if (a[i] != b[i]) return a[i] > b[i];
        }
        return false;
    }

    /** Android 13+ 必需: 未授予通知权限时系统会停发监听事件 */
    private void ensureNotificationPermission() {
        if (android.os.Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)
                    != android.content.pm.PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{android.Manifest.permission.POST_NOTIFICATIONS}, 100);
        }
    }

    private void refreshStatus() {
        boolean configured = !prefs.getString("server", "").isEmpty()
                && !prefs.getString("token", "").isEmpty();
        String last = prefs.getString("last_hb", "");
        // 监听绑定断开(系统省电策略): 心跳仍在线但通知收不到, 必须醒目提示
        // 门禁拦截期间服务尚未启动, 谈不上"断开", 该提示交给必要授权卡黄条
        boolean listenerDown = configured && serviceStarted && listenerEnabled()
                && !MonitorService.listenerBound;
        if (listenerDown) {
            statusDot.setBackgroundResource(R.drawable.dot_warn);
            statusText.setText("⚠ 监听已断开,重连中");
            tvHeart.setText("心跳仍在线但通知监听被系统暂停,若持续请重新开关一次「通知使用权」");
        } else if (configured && !last.isEmpty()) {
            statusDot.setBackgroundResource(R.drawable.dot_on);
            statusText.setText("设备在线");
            tvHeart.setText("最近心跳 " + last + " · 30 秒/次自动保活 · 监听正常");
        } else if (configured) {
            statusDot.setBackgroundResource(R.drawable.dot_warn);
            statusText.setText("连接中...");
            tvHeart.setText("正在等待首次心跳...");
        } else {
            statusDot.setBackgroundResource(R.drawable.dot_off);
            statusText.setText("未配置");
            tvHeart.setText("微信 / 支付宝 / QQ 收款自动监控上报");
        }
    }
}
