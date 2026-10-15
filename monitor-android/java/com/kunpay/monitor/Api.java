package com.kunpay.monitor;

import android.util.Log;

import org.json.JSONObject;

import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.net.URLEncoder;

/** 与 KunPay 平台 /monitor/heart、/monitor/push 协议交互 */
public final class Api {

    public interface Callback {
        void onResult(boolean ok, String msg);
    }

    private Api() {}

    /** POST 表单请求并返回响应体;token 走 body 不进 URL(避免服务端访问日志留痕);失败抛异常 */
    private static String post(String url, String form) throws Exception {
        HttpURLConnection conn = (HttpURLConnection) new URL(url).openConnection();
        conn.setConnectTimeout(8000);
        conn.setReadTimeout(8000);
        conn.setRequestMethod("POST");
        conn.setDoOutput(true);
        byte[] data = form.getBytes("UTF-8");
        conn.setFixedLengthStreamingMode(data.length);
        conn.setRequestProperty("Content-Type", "application/x-www-form-urlencoded");
        OutputStream os = conn.getOutputStream();
        os.write(data);
        os.flush();
        os.close();
        int code = conn.getResponseCode();
        InputStream in = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
        if (in == null) in = conn.getInputStream();
        ByteArrayOutputStream bos = new ByteArrayOutputStream();
        byte[] buf = new byte[4096];
        int n;
        while ((n = in.read(buf)) > 0) bos.write(buf, 0, n);
        in.close();
        String body = bos.toString("UTF-8");
        Log.d("KunPay", "POST " + url + " -> " + code + " " + body);
        return body;
    }

    private static String enc(String s) {
        try {
            return URLEncoder.encode(s, "UTF-8");
        } catch (Exception e) {
            return s;
        }
    }

    /** 心跳 */
    public static void heartbeat(String server, String token, final Callback cb) {
        new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    String body = post(trim(server) + "/monitor/heart", "token=" + enc(token));
                    JSONObject o = new JSONObject(body);
                    cb.onResult(o.optInt("code") == 1, body);
                } catch (Exception e) {
                    cb.onResult(false, String.valueOf(e.getMessage()));
                }
            }
        }).start();
    }

    /** 到账上报 */
    public static void push(String server, String token, final String type,
                            final String money, final String content, final Callback cb) {
        new Thread(new Runnable() {
            @Override
            public void run() {
                try {
                    String body = post(trim(server) + "/monitor/push",
                            "token=" + enc(token) + "&type=" + type
                                    + "&money=" + enc(money) + "&content=" + enc(content));
                    JSONObject o = new JSONObject(body);
                    boolean ok = o.optInt("code") >= 1;
                    cb.onResult(ok, body);
                } catch (Exception e) {
                    cb.onResult(false, String.valueOf(e.getMessage()));
                }
            }
        }).start();
    }

    private static String trim(String s) {
        return s == null ? "" : s.replaceAll("/+$", "");
    }
}
