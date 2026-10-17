package com.kunpay.monitor;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/** 保活接收器, 两个来源共用:
 *  1. 开机自启(BOOT_COMPLETED): 手机重启后自动恢复监控(云手机挂机场景)
 *  2. 看门狗闹钟(每5分钟): 进程意外被杀后拉活
 *  已配置(server+token)才拉起; 服务幂等, 重复启动无副作用。 */
public class KeepAliveReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        android.content.SharedPreferences p = context.getSharedPreferences("kunpay", Context.MODE_PRIVATE);
        boolean configured = !p.getString("server", "").isEmpty()
                && !p.getString("token", "").isEmpty();
        if (!configured) return;
        MonitorService.keepAliveNow(context);
    }
}
