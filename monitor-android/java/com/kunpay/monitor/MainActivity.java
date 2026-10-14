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
        Button btnListener = findViewById(R.id.btnListener);
        Button btnBattery = findViewById(R.id.btnBattery);
        View btnClear = findViewById(R.id.btnClear);

        // 恢复配置
        etServer.setText(prefs.getString("server", ""));
        etToken.setText(prefs.getString("token", ""));
        swAlipay.setChecked(prefs.getBoolean("ch_alipay", true));
        swWxpay.setChecked(prefs.getBoolean("ch_wxpay", true));
        swQqpay.setChecked(prefs.getBoolean("ch_qqpay", true));
        refreshLog();
        refreshStatus();

        // 支持 adb / 快捷方式带参初始化: am start ... --es server xx --es token xx
        Intent in = getIntent();
        if (in != null) {
            String sv = in.getStringExtra("server");
            String tk = in.getStringExtra("token");
            if (sv != null && !sv.isEmpty()) {
                etServer.setText(sv);
                prefs.edit().putString("server", sv).apply();
            }
            if (tk != null && !tk.isEmpty()) {
                etToken.setText(tk);
                prefs.edit().putString("token", tk).apply();
            }
        }

        btnSave.setOnClickListener(new View.OnClickListener() {
            @Override
            public void onClick(View v) {
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
                                    toast("连接成功");
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

    @Override
    public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode == 100) {
            refreshListenerState();
            ensureService(); // 授权后重连,让系统恢复监听事件投递
            toast("通知权限已更新");
        }
    }

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
        if (android.os.Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(android.Manifest.permission.POST_NOTIFICATIONS)
                    != android.content.pm.PackageManager.PERMISSION_GRANTED) {
            ensureNotificationPermission();
        }
    }

    @Override
    protected void onPause() {
        super.onPause();
        try {
            unregisterReceiver(logReceiver);
        } catch (Exception ignored) {
        }
    }

    private void ensureService() {
        // 触发监听服务启动(已授权时),保证心跳与监听随APP开启
        try {
            startService(new Intent(this, MonitorService.class));
        } catch (Exception ignored) {
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
        if (configured && !last.isEmpty()) {
            statusDot.setBackgroundResource(R.drawable.dot_on);
            statusText.setText("设备在线");
            tvHeart.setText("最近心跳 " + last + " · 30 秒/次自动保活");
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
