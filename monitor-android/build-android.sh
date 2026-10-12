#!/usr/bin/env bash
# KunPay Monitor 安卓端构建脚本(纯框架 API,免 Gradle)
# 依赖: JDK 17+、Android build-tools(aapt2/d8/zipalign/apksigner)、platforms;android-34
# 环境变量: ANDROID_HOME(默认 ~/.android-sdk)、JAVA_HOME(自动探测 ~/.jdks)
set -e
cd "$(dirname "$0")"

ANDROID_HOME="${ANDROID_HOME:-$HOME/.android-sdk}"
# 探测 JDK
if [ -z "$JAVA_HOME" ]; then
  for d in "$HOME"/.jdks/*/Contents/Home "$HOME"/.jdks/*; do
    [ -x "$d/bin/java" ] && JAVA_HOME="$d" && break
  done
fi
export JAVA_HOME
export PATH="$JAVA_HOME/bin:$ANDROID_HOME/platform-tools:$PATH"

BT=$(ls -d "$ANDROID_HOME"/build-tools/* 2>/dev/null | sort | tail -1)
PLATFORM=$(ls -d "$ANDROID_HOME"/platforms/android-* 2>/dev/null | sort -V | tail -1)
[ -z "$BT" ] && { echo "未找到 build-tools,请先安装 Android SDK"; exit 1; }
[ -z "$PLATFORM" ] && { echo "未找到 android 平台 jar"; exit 1; }
echo "==> build-tools: $BT"
echo "==> platform:    $PLATFORM"

OUT="$(pwd)/build"
rm -rf "$OUT"
mkdir -p "$OUT/classes" "$OUT/dex"

echo "==> 1/6 aapt2 编译资源"
"$BT/aapt2" compile --dir res -o "$OUT/res.zip"

echo "==> 2/6 aapt2 链接资源"
mkdir -p "$OUT/gen"
"$BT/aapt2" link -o "$OUT/app.unsigned.apk" \
  -I "$PLATFORM/android.jar" \
  --manifest AndroidManifest.xml \
  --min-sdk-version 26 --target-sdk-version 34 \
  --auto-add-overlay \
  --java "$OUT/gen" \
  "$OUT/res.zip"

echo "==> 3/6 javac 编译"
find java "$OUT/gen" -name "*.java" > "$OUT/sources.txt"
"$JAVA_HOME/bin/javac" -source 8 -target 8 -Xlint:-options \
  -bootclasspath "$PLATFORM/android.jar" \
  -classpath "$PLATFORM/android.jar" \
  -d "$OUT/classes" \
  @"$OUT/sources.txt"

echo "==> 4/6 d8 转 dex"
"$BT/d8" --release --lib "$PLATFORM/android.jar" --min-api 26 \
  --output "$OUT/dex" $(find "$OUT/classes" -name "*.class")
(cd "$OUT/dex" && zip -q -X "$OUT/app.unsigned.apk" classes.dex)

echo "==> 5/6 zipalign"
"$BT/zipalign" -f 4 "$OUT/app.unsigned.apk" "$OUT/app.aligned.apk"

echo "==> 6/6 签名"
KS="$OUT/debug.keystore"
if [ ! -f "$KS" ]; then
  "$JAVA_HOME/bin/keytool" -genkeypair -keystore "$KS" \
    -alias androiddebugkey -storepass android -keypass android \
    -dname "CN=KunPay Debug,O=KunPay,C=CN" -keyalg RSA -validity 10000
fi
"$BT/apksigner" sign --ks "$KS" --ks-pass pass:android --key-pass pass:android \
  --out KunPayMonitor.apk "$OUT/app.aligned.apk"
"$BT/apksigner" verify KunPayMonitor.apk && echo "签名校验通过"

echo "==> 完成: monitor-android/KunPayMonitor.apk"
ls -lh KunPayMonitor.apk
