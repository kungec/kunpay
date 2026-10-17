package service

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunzhifu/internal/model"
	"kunzhifu/internal/version"
)

const updateAPI = "https://api.github.com/repos/%s/releases/latest"

// 国内访问 GitHub 直连不稳定,内置加速镜像候选列表(按可用性自动切换)
var ghMirrors = []string{
	"",                      // 直连(优先)
	"https://gh-proxy.com/", // 国内常用加速
	"https://ghfast.top/",   // 备选
	"https://ghproxy.net/",  // 备选
}

// ghWorkingMirror 缓存当前可用的镜像下标(-1 = 未探测,0 = 直连)
var (
	ghMirrorMu  sync.Mutex
	ghMirrorIdx = -1
)

// ghURL 将 GitHub 原始 URL 包装为当前可用的访问地址。
// 优先使用管理员配置的 github_proxy 设置;未配置时使用上次探测成功的镜像。
func ghURL(rawURL string) string {
	proxy := strings.TrimSpace(strings.TrimRight(ghProxyVal, "/"))
	if proxy != "" {
		return proxy + "/" + rawURL
	}
	ghMirrorMu.Lock()
	idx := ghMirrorIdx
	ghMirrorMu.Unlock()
	if idx >= 0 && idx < len(ghMirrors) && ghMirrors[idx] != "" {
		return ghMirrors[idx] + rawURL
	}
	return rawURL
}

// ghProxyVal 包级缓存,由 Store.RefreshGhProxy 或启动时注入
var ghProxyVal string

// SetGhProxy 设置 GitHub 加速前缀
func SetGhProxy(v string) { ghProxyVal = strings.TrimRight(strings.TrimSpace(v), "/") }

// RefreshGhProxy 从数据库读取 github_proxy 设置并缓存(每次检查更新前调用)
func (s *Store) RefreshGhProxy() {
	if v := strings.TrimSpace(s.DB.MustSetting("github_proxy")); v != "" {
		SetGhProxy(v)
	}
}

// ghFetch 带自动镜像切换的 HTTP GET: 依次尝试直连和各镜像,成功后缓存
func ghFetch(rawURL string, timeout time.Duration) ([]byte, error) {
	ghMirrorMu.Lock()
	idx := ghMirrorIdx
	ghMirrorMu.Unlock()

	// 已有缓存的可用镜像 → 直接用
	if idx >= 0 {
		var tryURL string
		if idx < len(ghMirrors) && ghMirrors[idx] != "" {
			tryURL = ghMirrors[idx] + rawURL
		} else {
			tryURL = rawURL
		}
		data, err := httpGetWithTimeout(tryURL, timeout)
		if err == nil {
			return data, nil
		}
		// 缓存的镜像失效,重新探测
	}

	// 依次探测: 直连 → 各镜像
	for i, prefix := range ghMirrors {
		tryURL := prefix + rawURL
		data, err := httpGetWithTimeout(tryURL, timeout)
		if err == nil {
			ghMirrorMu.Lock()
			ghMirrorIdx = i
			ghMirrorMu.Unlock()
			return data, nil
		}
	}
	return nil, fmt.Errorf("所有 GitHub 镜像均不可达")
}

func httpGetWithTimeout(rawURL string, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "KunPay-Updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ReleaseInfo GitHub 最新 Release 摘要(缓存于设置 update_latest)
type ReleaseInfo struct {
	Tag       string `json:"tag"`
	Name      string `json:"name"`
	Notes     string `json:"notes"`
	URL       string `json:"url"`
	PublishAt string `json:"publish_at"`
	// 运行时字段(不落库)
	Newer bool `json:"-"`
}

// RepoName 当前用于检查更新的 GitHub 仓库
func (s *Store) RepoName() string { return repoOf(s) }

func repoOf(s *Store) string {
	if r := strings.TrimSpace(s.DB.MustSetting("update_repo")); r != "" {
		return r
	}
	return version.DefaultRepo
}

// CheckUpdate 拉取 GitHub 最新 Release 并缓存;返回是否发现比当前更新的版本
func (s *Store) CheckUpdate() (*ReleaseInfo, bool, error) {
	s.RefreshGhProxy()
	apiURL := fmt.Sprintf(updateAPI, repoOf(s))
	body, err := ghFetch(apiURL, 15*time.Second)
	if err != nil {
		return nil, false, fmt.Errorf("连接 GitHub 失败(已尝试直连与加速镜像): %v", err)
	}
	var gh struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		Body        string `json:"body"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
	}
	if err := json.Unmarshal(body, &gh); err != nil {
		return nil, false, err
	}
	info := &ReleaseInfo{
		Tag:       gh.TagName,
		Name:      gh.Name,
		Notes:     gh.Body,
		URL:       gh.HTMLURL,
		PublishAt: gh.PublishedAt,
	}
	if info.Name == "" {
		info.Name = info.Tag
	}
	blob, _ := json.Marshal(info)
	_ = s.DB.SetSetting("update_latest", string(blob))
	_ = s.DB.SetSetting("update_check_at", strconv.FormatInt(model.Now(), 10))
	newer := IsNewerVersion(info.Tag, version.Version)
	info.Newer = newer
	return info, newer, nil
}

// CachedUpdate 读取缓存的最新版本信息(不发网络请求)
func (s *Store) CachedUpdate() (*ReleaseInfo, bool) {
	raw := s.DB.MustSetting("update_latest")
	if raw == "" {
		return nil, false
	}
	var info ReleaseInfo
	if err := json.Unmarshal([]byte(raw), &info); err != nil {
		return nil, false
	}
	info.Newer = IsNewerVersion(info.Tag, version.Version)
	return &info, info.Newer
}

// UpdateCheckTask 后台任务: 每 6 小时拉取一次 GitHub Releases(静默失败)
func (s *Store) UpdateCheckTask() {
	if s.DB.MustSetting("update_check") != "1" {
		return
	}
	last := s.DB.MustSetting("update_check_at")
	lastAt, _ := strconv.ParseInt(last, 10, 64)
	if model.Now()-lastAt < 6*3600 {
		return
	}
	s.CheckUpdate()
}

// IsNewerVersion 比较语义化版本(latest 是否比 current 新),兼容 v 前缀与 1.2 短版本
func IsNewerVersion(latest, current string) bool {
	pa, pb := splitVer(latest), splitVer(current)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

// splitVer 解析主/次/修订号;无效输入返回负值标记
func splitVer(v string) [3]int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return [3]int{-1, -1, -1}
	}
	out := [3]int{}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return [3]int{-1, -1, -1}
	}
	for i, part := range parts {
		n := 0
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				break
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out
}

// ---------- 自动更新 ----------

// DoUpdate 下载指定版本的二进制并替换当前运行文件(不重启,由调用方决定重启方式)。
// 返回用户可读的状态消息。
//
// 安全模型: 二进制本体可经加速镜像下载, 但其 SHA-256 校验值只从信任链路获取
// (管理员配置的 github_proxy 或直连 api.github.com, 二者均有 TLS 证书校验)。
// 公共镜像不可信 —— 校验值拿不到或对不上时拒绝替换, 防止镜像投毒导致 RCE。
func (s *Store) DoUpdate(tag string) (string, error) {
	s.RefreshGhProxy()
	goos, goarch := runtime.GOOS, runtime.GOARCH

	var assetName string
	if goos == "windows" {
		assetName = fmt.Sprintf("kunpay_%s_%s.zip", goos, goarch)
	} else {
		assetName = fmt.Sprintf("kunpay_%s_%s.tar.gz", goos, goarch)
	}

	repo := repoOf(s)
	rawDL := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, tag, assetName)
	dlURL := ghURL(rawDL)

	// 信任链路取官方 SHA-256(不走公共自动镜像)
	wantDigest, err := s.assetDigest(repo, tag, assetName)
	if err != nil {
		return "", fmt.Errorf("无法校验更新包完整性(%v);已取消更新。"+
			"可到系统设置配置 GitHub 加速代理,或到 GitHub Releases 手动下载", err)
	}

	// 下载到临时文件(上限 512MB, 防异常响应塞满磁盘)
	tmpDir, err := os.MkdirTemp("", "kunpay-update-*")
	if err != nil {
		return "", fmt.Errorf("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpArchive := filepath.Join(tmpDir, assetName)
	client := SafeHTTPClient(120 * time.Second)
	resp, err := client.Get(dlURL)
	if err != nil {
		return "", fmt.Errorf("下载失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("下载返回 %d(请确认 %s 是否存在于该版本)", resp.StatusCode, assetName)
	}
	tmpFile, err := os.Create(tmpArchive)
	if err != nil {
		return "", err
	}
	if _, err = io.Copy(tmpFile, io.LimitReader(resp.Body, 512<<20)); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("下载写入失败: %v", err)
	}
	tmpFile.Close()

	// 完整性校验: 镜像链路的二进制必须与官方 digest 一致
	gotDigest, err := fileSHA256(tmpArchive)
	if err != nil {
		return "", fmt.Errorf("计算下载文件校验值失败: %v", err)
	}
	if !strings.EqualFold(gotDigest, wantDigest) {
		return "", fmt.Errorf("更新包校验失败(期望 sha256:%s, 实际 sha256:%s)——"+
			"下载链路可能被污染,已拒绝安装", wantDigest, gotDigest)
	}

	// 确定当前可执行文件路径
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取当前程序路径失败: %v", err)
	}
	exePath, _ = filepath.Abs(exePath)

	// 从压缩包中提取二进制到临时位置
	newBin := filepath.Join(tmpDir, "kunpay.new")
	if goos == "windows" {
		err = extractZipBinary(tmpArchive, newBin, "kunpay.exe")
	} else {
		err = extractTarGzBinary(tmpArchive, newBin, "kunpay")
	}
	if err != nil {
		return "", fmt.Errorf("解压失败: %v", err)
	}

	// 验证新二进制非空且可执行
	fi, err := os.Stat(newBin)
	if err != nil || fi.Size() < 1024 {
		return "", fmt.Errorf("解压后的二进制异常(大小 %d)", fiSize(newBin))
	}
	if goos != "windows" {
		os.Chmod(newBin, 0o755)
	}

	// 备份当前 → 替换为新版
	backupPath := exePath + ".bak"
	os.Remove(backupPath)
	if err := os.Rename(exePath, backupPath); err != nil {
		// Windows 上运行中的 exe 无法 rename,改用 copy+replace
		if err2 := copyFile(exePath, backupPath); err2 != nil {
			return "", fmt.Errorf("备份当前程序失败: %v", err2)
		}
		if err2 := copyFile(newBin, exePath); err2 != nil {
			return "", fmt.Errorf("替换程序失败: %v(Windows 可能需要手动停止程序后替换)", err2)
		}
		return "更新已写入,请手动重启程序以加载新版本", nil
	}
	if err := os.Rename(newBin, exePath); err != nil {
		// 回滚
		os.Rename(backupPath, exePath)
		return "", fmt.Errorf("替换程序失败(已回滚): %v", err)
	}

	return fmt.Sprintf("更新成功(备份: %s.bak),请重启服务加载新版本", exePath), nil
}

func fiSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// assetDigest 获取 GitHub Release 资产的官方 sha256(形如 "sha256:hex")。
// 只走信任链路: 管理员配置的 github_proxy(管理员自行信任)或直连 api.github.com
// (标准 TLS 证书校验); 刻意不用 ghMirrors 公共镜像 —— 镜像可被投毒, 不能作为校验锚。
func (s *Store) assetDigest(repo, tag, assetName string) (string, error) {
	api := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, tag)
	var body []byte
	var err error
	if proxy := strings.TrimSpace(ghProxyVal); proxy != "" {
		body, _ = httpGetWithTimeout(proxy+"/"+api, 20*time.Second)
	}
	if body == nil {
		// 未配置代理或代理失败 → 直连(信任锚)
		body, err = httpGetWithTimeout(api, 20*time.Second)
	}
	if err != nil {
		return "", err
	}
	var gh struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if jsonErr := json.Unmarshal(body, &gh); jsonErr != nil {
		return "", fmt.Errorf("解析 Release 信息失败: %v", jsonErr)
	}
	for _, a := range gh.Assets {
		if a.Name == assetName {
			if !strings.HasPrefix(a.Digest, "sha256:") || len(a.Digest) != len("sha256:")+64 {
				return "", fmt.Errorf("GitHub 未提供该资产的 sha256")
			}
			return strings.TrimPrefix(a.Digest, "sha256:"), nil
		}
	}
	return "", fmt.Errorf("该版本下未找到 %s", assetName)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// extractTarGzBinary 从 tar.gz 中提取指定名称的二进制文件
func extractTarGzBinary(archive, dest, binName string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("压缩包中未找到 %s", binName)
		}
		if err != nil {
			return err
		}
		// 匹配文件名(忽略目录前缀); 大小上限 256MB(防压缩炸弹塞满磁盘)
		base := filepath.Base(hdr.Name)
		if base == binName && hdr.Typeflag == tar.TypeReg {
			out, err := os.Create(dest)
			if err != nil {
				return err
			}
			if _, err = io.Copy(out, io.LimitReader(tr, 256<<20)); err != nil {
				out.Close()
				return err
			}
			out.Close()
			os.Chmod(dest, 0o755)
			return nil
		}
	}
}

// extractZipBinary 从 zip 中提取指定名称的二进制文件
func extractZipBinary(archive, dest, binName string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		base := filepath.Base(f.Name)
		if base == binName {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			out, err := os.Create(dest)
			if err != nil {
				return err
			}
			// 大小上限 256MB(防压缩炸弹)
			if _, err = io.Copy(out, io.LimitReader(rc, 256<<20)); err != nil {
				out.Close()
				return err
			}
			out.Close()
			return nil
		}
	}
	return fmt.Errorf("压缩包中未找到 %s", binName)
}
