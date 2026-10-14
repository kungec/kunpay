package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"kunzhifu/internal/model"
	"kunzhifu/internal/version"
)

const updateAPI = "https://api.github.com/repos/%s/releases/latest"

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
	url := fmt.Sprintf(updateAPI, repoOf(s))
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "KunPay-Updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false, fmt.Errorf("GitHub API 返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false, err
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
		if n == 0 && part != "" && !strings.HasPrefix(part, "0") && !strings.HasPrefix(part, "00") {
			// 保持 0 值语义
		}
		out[i] = n
	}
	return out
}
