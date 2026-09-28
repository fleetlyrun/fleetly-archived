package build

// 上传构建面（T 线 DT-6 / IMPL-T2-2）：「上下文 tar 包 + Dockerfile 入口 →
// buildkitd → zot，返回 digest 引用」的落盘与解包层。
//
// 信任级红线（票面硬约束）：上传上下文与 git 构建**同信任级**——解包产物
// 交给与 git 构建逐字相同的 dockerfile.v0 前端（无新增构建参数面）；本文件
// 只承担「把不可信 tar 安全地放到宿主盘上」的宿主侧防线：
//
//  1. 路径安全：绝对路径/`..`/反斜杠/卷名形态一律拒绝；先常规文件与目录、
//     后符号链与硬链的两遍解包——第二遍创建链接前核验祖先链无本次解出的
//     符号链、硬链目标必须是已解出的常规文件（tar 经典「先链后写」穿越
//     攻击在结构上不可达）；
//  2. 资源上限：外层读取器按 tar 字节强制上限（超限哨兵 ErrUploadTooLarge，
//     API 面映射 E_BUILD_UPLOAD_TOO_LARGE）；写入累计与条目数双上限兜底
//     （稀疏条目等非常规膨胀）；
//  3. 零残留：会话目录 <uploads root>/<build id>/，终态/失败/收敛/重启由
//     清理钩子（Builder defer、Queue 收敛路径、janitor mtime 兜底）删除；
//     CleanupUploadDir 只删上传根的直接子目录（直写 builds.request 的
//     任意删除形态在结构上不可达）。

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 上传面上限缺省（常量面；runtime 经 build.max_upload_mb 可下调/上调）。
const (
	// DefaultMaxUploadBytes 是上传 tar 字节总量上限缺省（256MiB——够承载
	// 函数 runner 上下文（KB 级）到中等仓库源码；与既有构建并发 2 组合
	// 的宿主盘敞口有界）。
	DefaultMaxUploadBytes = int64(256) << 20
	// MaxUploadChunkBytes 是单帧分片上限（1MiB；gRPC 缺省单帧接收上限
	// 4MiB 以下，不全局抬 MaxRecvMsgSize）。
	MaxUploadChunkBytes = 1 << 20
	// maxUploadEntries 是解包条目数上限（防元数据面目录炸弹；正常构建
	// 上下文远低于此）。
	maxUploadEntries = 100_000
	// maxUploadTrailingBytes 是 tar 结构结束后的余量上限（块对齐填充；
	// 超量 = 非法形态）。
	maxUploadTrailingBytes = 1 << 20
)

// 上传面哨兵错误（API 面据 errors.Is 映射注册码信封）。
var (
	// ErrUploadTooLarge 上传 tar 字节超过平台上限（流式累计即拒）。
	ErrUploadTooLarge = errors.New("build upload exceeds the platform size limit")
	// ErrUploadInvalid 上传上下文形态非法（tar 结构/路径穿越/入口缺失）。
	ErrUploadInvalid = errors.New("build upload context is invalid")
)

// UploadConfig 是上传构建面配置（api.BuildsService 消费；build.Config 的
// 归一投影——root 空 = 上传面未装配，RPC 如实报不可用）。
type UploadConfig struct {
	// Root 是上传会话根目录（<数据根>/build-uploads；受管根之一）。
	Root string
	// MaxBytes 是单次上传 tar 字节上限。
	MaxBytes int64
	// MaxChunkBytes 是单帧分片上限（协议面）。
	MaxChunkBytes int64
}

// UploadConfig 返回上传面配置投影（Normalize 后的根为绝对路径）。
func (c Config) UploadConfig() UploadConfig {
	return UploadConfig{Root: c.UploadsRoot, MaxBytes: c.MaxUploadBytes, MaxChunkBytes: MaxUploadChunkBytes}
}

// UploadSession 是一次上传构建的落盘会话（<root>/<build id>/context）。
// 生命周期：NewUploadSession → ExtractTar → API 入队构建（成功交接后由
// 构建终态清理钩子接管）；入队前失败由调用方 Cleanup。
type UploadSession struct {
	dir      string
	maxBytes int64
}

// NewUploadSession 创建会话目录（0700；会话目录按构建 ID 命名——清理
// 钩子以 request 的 ephemeral_dir 为唯一目标）。
func NewUploadSession(root, buildID string, maxBytes int64) (*UploadSession, error) {
	if root == "" {
		return nil, fmtErr("upload root is empty (upload face not assembled)")
	}
	if buildID == "" {
		return nil, fmtErr("upload session build id is empty")
	}
	if strings.ContainsAny(buildID, `/\`) || buildID == "." || buildID == ".." {
		return nil, fmtErr("upload session build id %q is not a plain path element", buildID)
	}
	dir := filepath.Join(root, buildID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmtErr("create upload session dir %s: %w", dir, err)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxUploadBytes
	}
	return &UploadSession{dir: dir, maxBytes: maxBytes}, nil
}

// Dir 返回会话目录（ephemeral_dir 落库值）。
func (s *UploadSession) Dir() string { return s.dir }

// ContextDir 返回上下文根目录（构建 request 的 context_dir 取值）。
func (s *UploadSession) ContextDir() string { return filepath.Join(s.dir, "context") }

// Cleanup 删除会话目录（幂等；入队前的失败路径调用方责任）。
func (s *UploadSession) Cleanup() error {
	if s == nil || s.dir == "" {
		return nil
	}
	if err := os.RemoveAll(s.dir); err != nil {
		return fmtErr("remove upload session dir %s: %w", s.dir, err)
	}
	return nil
}

// ExtractTar 从 r 流式解包上下文 tar 到会话的 context 目录（不落 tar 中间
// 文件）。返回解包字节数。错误归一 ErrUploadTooLarge / ErrUploadInvalid
// （调用方映射注册码信封；其他为宿主 IO 故障原样上抛）。
func (s *UploadSession) ExtractTar(r io.Reader) (int64, error) {
	contextDir := s.ContextDir()
	if err := os.Mkdir(contextDir, 0o700); err != nil {
		return 0, fmtErr("create upload context dir %s: %w", contextDir, err)
	}
	limiter := &uploadLimitReader{r: r, limit: s.maxBytes}
	tr := tar.NewReader(limiter)
	written := int64(0)
	entries := 0
	files := map[string]bool{} // 已解出的常规文件（硬链目标白名单）
	var links []pendingLink

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrUploadTooLarge) {
				return written, err
			}
			return written, fmt.Errorf("%w: read tar entry: %v", ErrUploadInvalid, err)
		}
		entries++
		if entries > maxUploadEntries {
			return written, fmt.Errorf("%w: more than %d entries", ErrUploadInvalid, maxUploadEntries)
		}
		name, err := cleanTarPath(hdr.Name)
		if err != nil {
			return written, err
		}
		if name == "" {
			continue // tar 根条目（"."/空名/尾斜杠根）
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := ensureContextDirs(contextDir, path.Dir(name), nil); err != nil {
				return written, err
			}
			target := filepath.Join(contextDir, filepath.FromSlash(name))
			if err := os.MkdirAll(target, 0o750); err != nil {
				return written, fmtErr("create upload dir %s: %w", target, err)
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA 是 v7 tar 旧式常规文件标记（读取侧兼容面；上游常量弃用但字节值仍需接受）
			if err := ensureContextDirs(contextDir, path.Dir(name), nil); err != nil {
				return written, err
			}
			target := filepath.Join(contextDir, filepath.FromSlash(name))
			if info, err := os.Lstat(target); err == nil && (info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
				return written, fmt.Errorf("%w: entry %q conflicts with existing %s", ErrUploadInvalid, hdr.Name, info.Mode())
			}
			mode := hdr.FileInfo().Mode().Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // G304/G110：target 是解包根内逐段校验后的派生路径，io.Copy 的字节量经 limiter 与写入累计双上限（ErrUploadTooLarge）——两者即本函数的解包安全契约
			if err != nil {
				return written, fmtErr("create upload file %s: %w", target, err)
			}
			n, cerr := io.Copy(f, tr) //nolint:gosec // G110 同上：限流读取器在流边界截断
			closeErr := f.Close()
			written += n
			if err := errors.Join(cerr, closeErr); err != nil {
				if errors.Is(err, ErrUploadTooLarge) {
					return written, err
				}
				return written, fmt.Errorf("%w: read tar entry %q: %v", ErrUploadInvalid, hdr.Name, err)
			}
			if written > s.maxBytes {
				return written, fmt.Errorf("%w: extracted bytes exceed the limit", ErrUploadTooLarge)
			}
			files[name] = true
		case tar.TypeSymlink:
			links = append(links, pendingLink{name: name, linkname: hdr.Linkname, symlink: true})
		case tar.TypeLink:
			links = append(links, pendingLink{name: name, linkname: hdr.Linkname, symlink: false})
		default:
			return written, fmt.Errorf("%w: entry %q has unsupported type %q", ErrUploadInvalid, hdr.Name, hdr.Typeflag)
		}
	}

	// tar 结构结束后读尽流余量（客户端流收尾：块对齐填充/尾部零块一并
	// 消费——不排空会让大上下文的客户端卡在发送窗口上）：总读取量仍经
	// limiter 计入同一上限，尾量本身另受独立上限约束。
	var trailing int64
	buf := make([]byte, 32*1024)
	for {
		n, rerr := limiter.Read(buf)
		trailing += int64(n)
		if trailing > maxUploadTrailingBytes {
			return written, fmt.Errorf("%w: trailing bytes after the tar end exceed %d", ErrUploadInvalid, maxUploadTrailingBytes)
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			if errors.Is(rerr, ErrUploadTooLarge) {
				return written, rerr
			}
			return written, fmt.Errorf("%w: drain upload stream: %v", ErrUploadInvalid, rerr)
		}
	}

	// 第二遍：链接类条目（符号链/硬链）——创建前核验祖先链无本次解出的
	// 符号链、目标白名单（硬链），穿越写入在结构上不可达。
	symlinks := map[string]bool{}
	for _, l := range links {
		if err := ensureContextDirs(contextDir, path.Dir(l.name), symlinks); err != nil {
			return written, err
		}
		target := filepath.Join(contextDir, filepath.FromSlash(l.name))
		if _, err := os.Lstat(target); err == nil {
			return written, fmt.Errorf("%w: link entry %q conflicts with an existing path", ErrUploadInvalid, l.name)
		}
		if l.symlink {
			if err := os.Symlink(l.linkname, target); err != nil {
				return written, fmtErr("create upload symlink %s: %w", target, err)
			}
			symlinks[l.name] = true
			continue
		}
		linkName, err := cleanTarPath(l.linkname)
		if err != nil || linkName == "" || !files[linkName] {
			return written, fmt.Errorf("%w: hardlink %q targets %q which is not an extracted regular file", ErrUploadInvalid, l.name, l.linkname)
		}
		if err := os.Link(filepath.Join(contextDir, filepath.FromSlash(linkName)), target); err != nil {
			return written, fmtErr("create upload hardlink %s: %w", target, err)
		}
	}
	return written, nil
}

// pendingLink 是一个待创建的链接条目（两遍解包第二遍消费）。
type pendingLink struct {
	name     string
	linkname string
	symlink  bool
}

// cleanTarPath 清洗 tar 条目名：拒绝绝对路径/卷名/反斜杠/含 NUL/不 clean
// 词形（`a/../b`）/`..` 前缀；空名与根条目返回空串（调用方跳过）。
func cleanTarPath(name string) (string, error) {
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: entry name contains NUL", ErrUploadInvalid)
	}
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("%w: entry name %q uses backslashes", ErrUploadInvalid, name)
	}
	raw := strings.TrimSuffix(name, "/")
	if raw == "" || raw == "." {
		return "", nil
	}
	cleaned := path.Clean(raw)
	if cleaned != raw || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: entry name %q escapes the upload root", ErrUploadInvalid, name)
	}
	if vol := filepath.VolumeName(filepath.FromSlash(cleaned)); vol != "" {
		return "", fmt.Errorf("%w: entry name %q carries a volume name", ErrUploadInvalid, name)
	}
	return cleaned, nil
}

// ensureContextDirs 在会话根内创建祖先目录链（逐级 MkdirAll，但每级核验
// 已存在条目不是符号链——第二遍解包时 symlinks 传入本次已创建的链集，
// 祖先链含链即拒绝；防「先解出符号链、再经其写子路径」的穿越形态）。
func ensureContextDirs(contextDir, relDir string, symlinks map[string]bool) error {
	if relDir == "" || relDir == "." {
		return nil
	}
	if relDir == ".." || strings.HasPrefix(relDir, "../") || strings.ContainsRune(relDir, '\\') {
		return fmt.Errorf("%w: parent directory %q escapes the upload root", ErrUploadInvalid, relDir)
	}
	parts := strings.Split(relDir, "/")
	cur := contextDir
	relParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: parent directory %q is not clean", ErrUploadInvalid, relDir)
		}
		cur = filepath.Join(cur, part)
		relParts = append(relParts, part)
		if symlinks != nil && symlinks[strings.Join(relParts, "/")] {
			return fmt.Errorf("%w: parent directory %q is a symlink extracted from this upload", ErrUploadInvalid, strings.Join(relParts, "/"))
		}
		if info, err := os.Lstat(cur); err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: parent directory %q is a symlink", ErrUploadInvalid, part)
			}
			if !info.IsDir() {
				return fmt.Errorf("%w: parent path %q is not a directory", ErrUploadInvalid, part)
			}
			continue
		}
		if err := os.Mkdir(cur, 0o750); err != nil && !os.IsExist(err) {
			return fmtErr("create upload dir %s: %w", cur, err)
		}
	}
	return nil
}

// uploadLimitReader 是按字节上限截断的流读取器：读满 limit 后仍被要求
// 提供数据即返回 ErrUploadTooLarge（精确等于上限且底层已 EOF 的形态不误报
// ——EOF 是合法收尾）。
type uploadLimitReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (l *uploadLimitReader) Read(p []byte) (int, error) {
	if l.read >= l.limit {
		var one [1]byte
		n, err := l.r.Read(one[:])
		if n > 0 {
			return 0, fmt.Errorf("%w (limit %d bytes)", ErrUploadTooLarge, l.limit)
		}
		if err != nil {
			return 0, err
		}
		return 0, io.ErrNoProgress // 底层 0,nil 形态（理论不可达）
	}
	if int64(len(p)) > l.limit-l.read {
		p = p[:l.limit-l.read]
	}
	n, err := l.r.Read(p)
	l.read += int64(n)
	return n, err
}

// ValidateUploadDockerfile 校验 Dockerfile 入口（相对上下文根的仓内路径；
// 空 → Dockerfile）并返回规范化相对路径。规则：clean 相对路径（允许
// `./` 前缀折叠）、不越出上下文根、符号链解析后仍在根内、必须是常规文件。
func ValidateUploadDockerfile(contextDir, dockerfile string) (string, error) {
	if strings.ContainsRune(dockerfile, 0) || strings.ContainsRune(dockerfile, '\\') {
		return "", fmt.Errorf("%w: dockerfile path %q is invalid", ErrUploadInvalid, dockerfile)
	}
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	cleaned := path.Clean(dockerfile)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return "", fmt.Errorf("%w: dockerfile path %q escapes the context root", ErrUploadInvalid, dockerfile)
	}
	target := filepath.Join(contextDir, filepath.FromSlash(cleaned))
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("%w: dockerfile %q not found in the uploaded context: %v", ErrUploadInvalid, cleaned, err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(contextDir)
	if err != nil {
		return "", fmt.Errorf("%w: resolve context root: %v", ErrUploadInvalid, err)
	}
	if !containsPath(resolvedRoot, resolved) {
		return "", fmt.Errorf("%w: dockerfile %q resolves outside the context root", ErrUploadInvalid, cleaned)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%w: dockerfile %q not readable: %v", ErrUploadInvalid, cleaned, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: dockerfile %q is not a regular file", ErrUploadInvalid, cleaned)
	}
	return cleaned, nil
}

// CleanupUploadDir 删除上传会话目录（终态清理钩子）。只接受上传根的**直接
// 子目录实体**：非在根内、等于根、带路径分隔的深层形态、符号链条目一律
// no-op——直写 builds.request 伪造 ephemeral_dir 的任意删除不可达。返回
// 错误仅表示删除失败（no-op 返回 nil）。
func CleanupUploadDir(dir, uploadsRoot string) error {
	if dir == "" || uploadsRoot == "" {
		return nil
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return fmtErr("resolve upload dir %s: %w", dir, err)
	}
	absRoot, err := filepath.Abs(uploadsRoot)
	if err != nil {
		return fmtErr("resolve uploads root %s: %w", uploadsRoot, err)
	}
	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil || rel == "." || rel == ".." ||
		strings.ContainsAny(rel, `/\`) || strings.ContainsRune(rel, 0) {
		return nil // 不含目标（越界/根自身/深层形态）：静默拒绝
	}
	info, err := os.Lstat(absDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmtErr("stat upload dir %s: %w", absDir, err)
	}
	if !info.IsDir() {
		return nil // 符号链/文件条目：不删（只删本平台创建的实体目录）
	}
	if err := os.RemoveAll(absDir); err != nil {
		return fmtErr("remove upload dir %s: %w", absDir, err)
	}
	return nil
}
