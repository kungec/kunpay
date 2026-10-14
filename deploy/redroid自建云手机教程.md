# redroid 自建安卓云手机教程(Linux 服务器)

> 在自己的 Linux 服务器上,用 Docker 跑一个个完整安卓容器 —— 每个容器就是一台
> "云手机",装上 KunPay Monitor 即可 7×24 监控收款,无需任何实体手机。
> 配套文件: [docker-compose.yml](redroid/docker-compose.yml) · [init-monitor.sh](redroid/init-monitor.sh)

---

## 一、服务器要求

| 项 | 要求 |
|---|---|
| 系统 | **Ubuntu 20.04 / 22.04**(内核自带 binder 模块,redroid 官方推荐) |
| 内核 | 加载 binder(见第二步);部分云主机需选 Ubuntu 官方内核机型 |
| Docker | 已安装 |
| 资源 | 每个安卓实例约 1~2GB 内存 + 4~6GB 磁盘;8核16G 可跑 5~8 个 |
| 架构 | **ARM 服务器最省心**(微信/支付宝原生直跑);x86 服务器需带转译的镜像(见 3.2) |

---

## 二、开启内核 binder 支持(Ubuntu)

```bash
sudo apt install linux-modules-extra-$(uname -r)
sudo modprobe binder_linux devices="binder,hwbinder,vndbinder"
# 开机自动加载
echo binder_linux | sudo tee /etc/modules-load.d/binder.conf
# 验证
ls /dev/binderfs 2>/dev/null || ls /dev/binder
```

---

## 三、启动第一台"云手机"

### 3.1 单实例快速启动(320×640 小屏,资源占用最低)

```bash
docker run -itd --rm --privileged --name kunpay-cloud1 \
  -v /data/cloud1:/data \
  -p 5555:5555 \
  redroid/redroid:13.0.0-latest \
  androidboot.redroid_width=320 \
  androidboot.redroid_height=640 \
  androidboot.redroid_dpi=160 \
  androidboot.redroid_gpu_mode=guest
```

> `gpu_mode=guest` 为软件渲染,无 GPU 服务器适用;小屏 320×640 与
> KunPay Monitor 的 UI 自动化坐标完全匹配(见第四步脚本)。

### 3.2 x86 服务器跑微信/支付宝(ARM 应用转译)

微信/支付宝是 ARM 应用,x86 容器需带转译层的镜像(二选一按 CPU):

```bash
# Intel CPU — houdini 转译
redroid/redroid:13.0.0_4440a64e_with_houdini.BV3N0P0
# AMD CPU — ndk 转译
redroid/redroid:13.0.0_4440a64e_with_ndk.AV0M0P0
```

ARM 服务器直接用 `redroid/redroid:13.0.0-arm64-latest`,原生直跑无需转译。

---

## 四、初始化: 安装监控 APP 并自动配置

仓库提供一键脚本 [redroid/init-monitor.sh](redroid/init-monitor.sh):

```bash
cd deploy/redroid
./init-monitor.sh 5555 "http://你的平台地址" "设备Token"
```

脚本自动完成: adb 连接容器 → 安装 KunPay Monitor → 授予通知权限
(POST_NOTIFICATIONS + 通知使用权)→ 启动 APP 并自动填写平台地址与 Token → 保存。

完成后验证:

```bash
adb -s 127.0.0.1:5555 shell pidof com.kunpay.monitor   # 进程存活
# 平台后台「监控设备」中该设备显示在线(心跳 30 秒/次)
```

---

## 五、登录微信/支付宝(需要看到屏幕)

容器是无头(headless)的,账号登录需要扫码/输密码——用 **scrcpy** 把容器屏幕
投到你的电脑上操作:

```bash
# 电脑上安装 scrcpy(https://github.com/Genymobile/scrcpy)
scrcpy -s 127.0.0.1:5555
```

在投屏窗口里: 安装并登录微信/支付宝(容器无谷歌服务,微信/支付宝不需要 GMS ✓)。
登录完成后即可退出 scrcpy,监控 APP 在容器内持续工作。

---

## 六、多实例集群(多账号轮询)

用配套的 [docker-compose.yml](redroid/docker-compose.yml) 一键拉起 N 台:

```bash
cd deploy/redroid
docker compose up -d     # 默认 3 台: 15555/15556/15557 端口
```

然后对每台执行第四步初始化(端口分别 15555/15556/15557,Token 各自不同)。
对应关系: 每个容器登录一个收款账号 = 后台一台监控设备,参与渠道轮询/主备接管。

---

## 七、日常运维

| 操作 | 命令 |
|---|---|
| 查看容器 | `docker ps` |
| 重启某台云手机 | `docker restart kunpay-cloud1` |
| scrcpy 投屏 | `scrcpy -s 127.0.0.1:5555` |
| 容器内截屏 | `adb -s 127.0.0.1:5555 exec-out screencap -p > s.png` |
| 升级监控 APP | 重新执行第四步脚本(会覆盖安装) |

---

## 八、注意事项

1. **微信/支付宝风控**: 云端环境登录收款账号可能触发验证,建议固定 IP、
   先人工登录并正常使用几天"养号",再挂监控
2. **转译层性能**: x86 转译跑微信可用但非原生,启动慢属正常;追求原生选 ARM 服务器
3. **数据持久化**: `-v /data/cloudN:/data` 已把安卓数据落在宿主机目录,
   容器删除重建(不带 --rm)数据仍在
4. **不要跑在个人电脑**: 云手机需要 7×24 开机,请使用服务器
