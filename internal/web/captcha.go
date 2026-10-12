package web

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	fixed "golang.org/x/image/math/fixed"
)

// 自绘图形验证码: 4位字符 + 噪点干扰线,一次性使用,5 分钟过期。
// 纯 Go 实现(官方 x/image 模块),无任何外部依赖,保持单文件部署。

const captchaCharset = "23456789abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ"

type captchaEntry struct {
	code string
	exp  time.Time
}

var (
	captchaMu    sync.Mutex
	captchaStore = map[string]captchaEntry{}
)

func randCaptchaCode(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = captchaCharset[rand.Intn(len(captchaCharset))]
	}
	return string(b)
}

// newCaptcha 生成验证码,返回 (id, 图形PNG)
func newCaptcha() (string, []byte) {
	captchaMu.Lock()
	now := time.Now()
	for k, e := range captchaStore {
		if e.exp.Before(now) {
			delete(captchaStore, k)
		}
	}
	id := dbRandKey6()
	code := randCaptchaCode(4)
	captchaStore[id] = captchaEntry{code: code, exp: now.Add(5 * time.Minute)}
	captchaMu.Unlock()
	png, err := drawCaptchaPNG(code)
	if err != nil {
		// 渲染失败兜底: 返回纯色小图(校验仍以存储的 code 为准)
		png = solidPNG()
	}
	return id, png
}

// verifyCaptcha 校验并销毁(一次性)
func verifyCaptcha(id, input string) bool {
	captchaMu.Lock()
	e, ok := captchaStore[id]
	if ok {
		delete(captchaStore, id)
	}
	captchaMu.Unlock()
	if !ok || e.exp.Before(time.Now()) {
		return false
	}
	return strings.EqualFold(e.code, strings.TrimSpace(input))
}

func dbRandKey6() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 20)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

func solidPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 260, 88))
	for y := 0; y < 88; y++ {
		for x := 0; x < 260; x++ {
			img.Set(x, y, color.RGBA{240, 243, 248, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// drawChar 绘制单个字符(含字号与位置)
func drawChar(img *image.RGBA, c color.Color, x, y int, r rune) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: basicfont.Face7x13,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y + 13)}, // 基线位置
	}
	d.DrawString(string(r))
}

// drawCaptchaPNG 渲染带干扰的验证码图
func drawCaptchaPNG(code string) ([]byte, error) {
	const (
		baseW, baseH = 150, 44
		scale        = 2
		W, H         = baseW * scale, baseH * scale
	)
	base := image.NewRGBA(image.Rect(0, 0, baseW, baseH))
	for y := 0; y < baseH; y++ {
		for x := 0; x < baseW; x++ {
			base.Set(x, y, color.RGBA{240, 243, 248, 255})
		}
	}
	// 字符: 每个字符画到独立小图,2 倍放大后错落贴入
	x := 18
	for _, ch := range code {
		charImg := image.NewRGBA(image.Rect(0, 0, 10, 16))
		drawChar(charImg, color.RGBA{
			uint8(30 + rand.Intn(60)), uint8(40 + rand.Intn(60)), uint8(120 + rand.Intn(90)), 255,
		}, 1, 1, ch)
		dst := image.NewRGBA(image.Rect(0, 0, 20, 32))
		xdraw.NearestNeighbor.Scale(dst, dst.Bounds(), charImg, charImg.Bounds(), draw.Over, nil)
		jitterY := rand.Intn(7) - 3
		jitterX := rand.Intn(5) - 2
		draw.Draw(base, image.Rect(x+jitterX, 8+jitterY, x+jitterX+20, 8+jitterY+32), dst, image.Point{}, draw.Over)
		x += 30
	}
	// 干扰线
	for i := 0; i < 3; i++ {
		c := color.RGBA{uint8(120 + rand.Intn(90)), uint8(130 + rand.Intn(80)), uint8(200 + rand.Intn(55)), 120}
		drawLine(base, rand.Intn(baseW), rand.Intn(baseH), rand.Intn(baseW), rand.Intn(baseH), c)
	}
	// 噪点
	for i := 0; i < 220; i++ {
		base.Set(rand.Intn(baseW), rand.Intn(baseH), color.RGBA{
			uint8(rand.Intn(200)), uint8(rand.Intn(200)), uint8(rand.Intn(220)), uint8(60 + rand.Intn(120)),
		})
	}

	final := image.NewRGBA(image.Rect(0, 0, W, H))
	xdraw.NearestNeighbor.Scale(final, final.Bounds(), base, base.Bounds(), draw.Src, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, final); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawLine 有界插值画线(固定步数,绝不死循环)
func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.Color) {
	steps := abs(x1 - x0)
	if dy := abs(y1 - y0); dy > steps {
		steps = dy
	}
	if steps == 0 {
		img.Set(x0, y0, c)
		return
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		img.Set(x0+int(math.Round(float64(x1-x0)*t)), y0+int(math.Round(float64(y1-y0)*t)), c)
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// handleCaptchaImage 输出验证码图片(no-store,点击可刷新)
func handleCaptchaImage(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	captchaMu.Lock()
	e, ok := captchaStore[id]
	captchaMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	data, err := drawCaptchaPNG(e.code)
	if err != nil {
		http.Error(w, "生成失败", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// —— 测试辅助(仅测试构建/联调使用) ——
var lastCode string

func NewCaptchaForTest() (string, []byte) {
	id, png := newCaptcha()
	captchaMu.Lock()
	lastCode = captchaStore[id].code
	captchaMu.Unlock()
	return id, png
}

func LastCodeForTest() string { return lastCode }
func VerifyCaptchaForTest(id, code string) bool { return verifyCaptcha(id, code) }
