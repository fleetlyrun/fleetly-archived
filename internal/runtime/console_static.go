package runtime

// Console 静态托管（T2.21）：gateway 在 /ui/ 前缀托管 Console SPA 构建产物
// （console.static_dir 显式指向；未配置时回落镜像内置 consoleBakedDir——
// 存在才启用，见该常量注记）。鉴权豁免**精确到 /ui/
// 前缀**（静态资源不要求 token；数据面仍全部走 /v1 鉴权）——本文件是该
// 豁免的唯一实现位，登记见 gateway.go 的原生端点例外清单。

import (
	"compress/gzip"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// consoleUIPathPrefix 是 Console 静态托管的唯一 URL 前缀。分派面 = 豁免面：
// newRootHandler 只把该前缀的请求交给静态 handler，其余路径原样进 gateway
// mux（无 token 仍 401）。
const consoleUIPathPrefix = "/ui"

// consoleBakedDir 是容器形态烤入镜像的 Console 构建产物路径（deploy/
// Dockerfile.fleetlyd 的 console 构建层产出，2026-09-29 起 console 进镜像）。
// console.static_dir 未显式配置时该目录存在（含 index.html）即启用——容器
// 形态开箱即有 /ui/；原生形态不安装此目录，「缺省关闭」口径不变。显式配置
// 恒优先（含显式配置但目录缺失的 fail-fast）。
const consoleBakedDir = "/opt/fleetly/console"

// consoleDirOrDefault 解析 Console 静态根：显式配置优先；未配置时回落
// baked 目录（存在才启用）。baked 作参数注入供测试钉语义。
func consoleDirOrDefault(configured, baked string) string {
	if configured != "" {
		return configured
	}
	if _, err := os.Stat(filepath.Join(baked, "index.html")); err == nil {
		return baked
	}
	return ""
}

// consoleInlineThemeScriptHash 是 index.html 内联「防闪主题」脚本的 CSP
// 静态哈希（sha256-'…'，script-src 白名单项）。背景（2026-09-25 走查）：
// consoleCSP 无 script-src 时回落 default-src 'self'，该内联脚本每页被
// 拦 2 条 console error（主题防闪失效）；脚本必须内联在 React 挂载前执行
// 才有防闪意义，外链化得不偿失——按脚本原始字节（console/index.html 被
// .gitattributes 钉 LF，构建产物逐字节确定）计算哈希放行。
// 纪律：改动 index.html 的内联脚本（哪怕一个空格/换行）必须重算并同步
// 本哈希，否则脚本被 CSP 静默拦截；计算方式 = sha256（<script> 与
// </script> 之间的原始字节，含换行与缩进，不含标签本身）。
const consoleInlineThemeScriptHash = "sha256-+ZMaiU8bP+f4HWQQalPITs6U4z+vCsPp7gQFCyWhSqc="

// consoleCSP 是 /ui/ 静态面的 Content-Security-Policy（D4-④）。指令集按
// Console 构建产物的实际加载形态定稿（2026-09-19 对 dist/ 排查）：Vite
// 产物为外部 module script + 外部样式表，数据面 fetch/流式全走同源 /v1，
// 图标为同源 svg——无 'unsafe-inline' 脚本面。
//   - script-src 'self' + 内联主题脚本哈希白名单（见
//     consoleInlineThemeScriptHash 注释）：显式声明优于 default-src 回落，
//     防闪脚本按哈希精确放行、其余内联脚本仍拒（2026-09-25 修订）；
//   - connect-src 'self'：/v1 REST + NDJSON 流（VITE_API_BASE 指向跨源
//     控制面时需放宽本指令）；
//   - img-src 'self' data:：同源 favicon/图标，data: 为零散内联图标预留；
//   - style-src 'self' 'unsafe-inline'：外部样式表之外，xterm（E7 Web 终端）
//     的 DOM 渲染器在运行时注入 <style> 元素下发主题色/光标/行列样式——
//     无 'unsafe-inline' 时该注入被静默拦截，终端默认前景色回落到页面
//     文字色（深色主题下即黑底黑字，光标亦不可见；staging 真机实测）。
//     样式注入不含执行面（相对 script 的 unsafe-inline 风险低一个量级），
//     React 自身的样式修改仍走 CSSOM 不受影响（2026-09-25 修订）。
const consoleCSP = "default-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; script-src 'self' '" + consoleInlineThemeScriptHash + "'"

// consoleCompressibleExt 是按扩展名的可压缩静态资产（2026-09-25 加载优化
// ——staging 实测 daemon 静态面无压缩，浏览器实传 1MB 未压缩 JS；gzip 后
// 传输约 1/4）。woff2/jpg/png 等本身已压缩的格式不在此列（再压缩无收益）。
var consoleCompressibleExt = map[string]bool{
	".js":   true,
	".css":  true,
	".html": true,
	".svg":  true,
	".json": true,
	".map":  true,
	".txt":  true,
}

// newConsoleUIHandler 构造 Console SPA 静态托管 handler（/ui/ 前缀的分派
// 目标）：
//   - 命中目录内真实文件 → 按扩展名 Content-Type 原样托管（fs.ValidPath
//     拒绝 ..、绝对路径等形态——目录外不可达，无穿越面）；
//   - 未命中（SPA 深链 /ui/apps/xyz 等）→ 回退 index.html（200，前端
//     router 接管）；
//   - 目录缺 index.html 在装配期 fail-fast（Console 未构建即配置启用属
//     配置错误，拒绝启动而非运行期 500）；
//   - 可压缩资产按 Accept-Encoding 协商 gzip（透明包装 ResponseWriter——
//     Content-Length 与压缩流长度必然不一致，压缩时删除；范围面只此静态
//     前缀，流式 API 不经此路径）。
func newConsoleUIHandler(dir string) (http.Handler, error) {
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		return nil, fmt.Errorf("console.static_dir %q has no index.html (run `pnpm build` in console/ first): %w", dir, err)
	}
	fsys := os.DirFS(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CSP 对真实文件与 SPA 回退（index.html）一律生效（见 consoleCSP 注释）。
		w.Header().Set("Content-Security-Policy", consoleCSP)
		name := strings.TrimPrefix(r.URL.Path, consoleUIPathPrefix)
		name = strings.TrimPrefix(name, "/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		// 目录不托管（无列表面）：目录形态与未命中同样走 SPA 回退。
		serveName := name
		if st, err := fs.Stat(fsys, name); err != nil || st.IsDir() {
			serveName = "index.html"
		}
		w, done := wrapConsoleGzip(w, r, serveName)
		defer done()
		//nolint:gosec // G703：serveName 已过 fs.ValidPath（拒绝 ..、绝对路径等形态）——fsys 根定，目录外不可达
		http.ServeFileFS(w, r, fsys, serveName)
	}), nil
}

// wrapConsoleGzip 对可压缩资产 + 客户端声明 gzip 的请求包装压缩写面；返回
// 的 done 在 handler 返回时冲刷 gzip 尾部（不压缩路径为 no-op）。
func wrapConsoleGzip(w http.ResponseWriter, r *http.Request, name string) (http.ResponseWriter, func()) {
	if !consoleCompressibleExt[strings.ToLower(filepath.Ext(name))] {
		return w, func() {}
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		return w, func() {}
	}
	gzw := &consoleGzipWriter{ResponseWriter: w, gw: gzip.NewWriter(nil)}
	return gzw, func() {
		if gzw.compress {
			//nolint:errcheck,gosec // G104：静态资产收尾冲刷——失败只能意味着连接已断
			_ = gzw.gw.Close()
		}
	}
}

// consoleGzipWriter 是协商后的压缩写面：WriteHeader 时定案（2xx 才压缩——
// 304 无 body、4xx 短响应无收益），压缩则删 Content-Length、声明
// Content-Encoding 并登记 Vary（缓存层按协商头分流）。
type consoleGzipWriter struct {
	http.ResponseWriter
	gw          *gzip.Writer
	compress    bool
	wroteHeader bool
}

func (c *consoleGzipWriter) WriteHeader(code int) {
	if c.wroteHeader {
		return
	}
	c.wroteHeader = true
	if code >= 200 && code < 300 {
		h := c.Header()
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		c.compress = true
		c.gw.Reset(c.ResponseWriter)
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *consoleGzipWriter) Write(p []byte) (int, error) {
	if !c.wroteHeader {
		c.WriteHeader(http.StatusOK)
	}
	if c.compress {
		return c.gw.Write(p)
	}
	return c.ResponseWriter.Write(p)
}
