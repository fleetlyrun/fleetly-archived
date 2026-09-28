package build

// 上传构建面（DT-6/IMPL-T2-2）单测：tar 解包的宿主侧安全防线（路径穿越/
// 链接穿越/设备条目）、大小上限、Dockerfile 入口校验、会话清理与受根约束
// 的 RemoveAll、配置归一（上传根并入受管根）。

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarEntry 是测试 tar 的一个条目。
type tarEntry struct {
	name     string
	linkname string
	typeflag byte
	body     string
}

// buildTar 构造测试 tar 字节流。
func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typeflag
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := int64(0o644)
		size := int64(len(e.body))
		if typ == tar.TypeDir {
			mode = 0o755
			size = 0
		}
		mode |= int64(typ)
		if typ == tar.TypeSymlink {
			mode = 0o777 | int64(tar.TypeSymlink)
		}
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     mode,
			Size:     size,
			Typeflag: typ,
			Linkname: e.linkname,
		}
		switch typ {
		case tar.TypeSymlink:
			hdr.Size = 0
		case tar.TypeLink:
			hdr.Size = 0
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %s: %v", e.name, err)
		}
		if size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write tar body %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// newSession 构造测试会话（root 临时目录 + 由调用方指定上限）。
func newSession(t *testing.T, maxBytes int64) *UploadSession {
	t.Helper()
	s, err := NewUploadSession(t.TempDir(), "01TESTUPLOAD000000000000000", maxBytes)
	if err != nil {
		t.Fatalf("NewUploadSession: %v", err)
	}
	return s
}

// TestUploadExtractTarHappyPath 常规解包：文件/目录逐类落盘，内容与嵌套
// 目录自动创建正确；符号链/硬链在宿主支持时追加验证（Windows 无特权环境
// 跳过该腿——解包器的链接安全语义由独立用例覆盖）。
func TestUploadExtractTarHappyPath(t *testing.T) {
	s := newSession(t, DefaultMaxUploadBytes)
	raw := buildTar(t,
		tarEntry{name: "Dockerfile", body: "FROM scratch\nCOPY app.js /app.js\n"},
		tarEntry{name: "src", typeflag: tar.TypeDir},
		tarEntry{name: "src/app.js", body: "console.log('hi')\n"},
		tarEntry{name: "run/deep/nested.txt", body: "nested"},
	)
	if _, err := s.ExtractTar(bytes.NewReader(raw)); err != nil {
		t.Fatalf("ExtractTar: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(s.ContextDir(), "Dockerfile")); err != nil || !strings.HasPrefix(string(got), "FROM scratch") {
		t.Fatalf("Dockerfile = %q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(s.ContextDir(), "run", "deep", "nested.txt")); err != nil || string(got) != "nested" {
		t.Fatalf("nested.txt = %q err=%v", got, err)
	}

	if !symlinksSupported(t) {
		t.Log("symlink creation unsupported on this host: link legs skipped")
		return
	}
	s2 := newSession(t, DefaultMaxUploadBytes)
	raw = buildTar(t,
		tarEntry{name: "src/app.js", body: "console.log('hi')\n"},
		tarEntry{name: "link.js", typeflag: tar.TypeSymlink, linkname: "src/app.js"},
		tarEntry{name: "hard.js", typeflag: tar.TypeLink, linkname: "src/app.js"},
	)
	if _, err := s2.ExtractTar(bytes.NewReader(raw)); err != nil {
		t.Fatalf("ExtractTar(links): %v", err)
	}
	if target, err := os.Readlink(filepath.Join(s2.ContextDir(), "link.js")); err != nil || target != "src/app.js" {
		t.Fatalf("symlink target = %q err=%v", target, err)
	}
	if got, err := os.ReadFile(filepath.Join(s2.ContextDir(), "hard.js")); err != nil || string(got) != "console.log('hi')\n" {
		t.Fatalf("hardlink content = %q err=%v", got, err)
	}
}

// symlinksSupported 报告宿主是否可创建符号链（Windows 无特权环境为 false）。
func symlinksSupported(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		return false
	}
	return true
}

// TestUploadExtractTarRejectsUnsafeNames 路径形态拒绝：绝对路径/`..`/
// 反斜杠/NUL/不 clean 词形一律 ErrUploadInvalid，且根外零写入。
func TestUploadExtractTarRejectsUnsafeNames(t *testing.T) {
	cases := []string{
		"/etc/passwd",
		"../escape.txt",
		"a/../../escape.txt",
		"a/../b",
		`a\..\..\escape.txt`,
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSession(t, DefaultMaxUploadBytes)
			if _, err := s.ExtractTar(bytes.NewReader(buildTar(t, tarEntry{name: name, body: "x"}))); !errors.Is(err, ErrUploadInvalid) {
				t.Fatalf("ExtractTar(%q) err = %v, want ErrUploadInvalid", name, err)
			}
			// 上游目录（会话根的父）零残留写入。
			parent := filepath.Dir(s.Dir())
			entries, rerr := os.ReadDir(parent)
			if rerr != nil {
				t.Fatalf("read session parent: %v", rerr)
			}
			for _, e := range entries {
				if e.Name() != filepath.Base(s.Dir()) {
					t.Fatalf("unexpected write outside session: %s", e.Name())
				}
			}
		})
	}
	// NUL 词形由清洗谓词直接拒绝（tar 编码器不接受 NUL 名，不能经 tar 构造）。
	if _, err := cleanTarPath("a\x00b"); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("cleanTarPath(NUL) err = %v, want ErrUploadInvalid", err)
	}
}

// TestUploadExtractTarRejectsSymlinkParentTraversal 经典「先链后写」穿越：
// 符号链条目的子路径文件在链创建前已解到根内，链创建时与实体冲突 →
// 整体拒绝；根外（含链目标）零写入。
func TestUploadExtractTarRejectsSymlinkParentTraversal(t *testing.T) {
	s := newSession(t, DefaultMaxUploadBytes)
	// 链目标指向会话根之外（绝对路径）；子文件 evil/pwned 先解为根内实体。
	outside := t.TempDir()
	raw := buildTar(t,
		tarEntry{name: "evil", typeflag: tar.TypeSymlink, linkname: outside},
		tarEntry{name: "evil/pwned", body: "pwned"},
	)
	if _, err := s.ExtractTar(bytes.NewReader(raw)); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("ExtractTar err = %v, want ErrUploadInvalid (symlink/dir conflict)", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("write escaped the upload root: %v", err)
	}
}

// TestUploadExtractTarRejectsDeviceEntries 设备/FIFO/未知类型条目拒绝
//（构建上下文不需要，属攻击面收口）。
func TestUploadExtractTarRejectsDeviceEntries(t *testing.T) {
	s := newSession(t, DefaultMaxUploadBytes)
	raw := buildTar(t, tarEntry{name: "dev", typeflag: tar.TypeChar})
	if _, err := s.ExtractTar(bytes.NewReader(raw)); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("ExtractTar(char device) err = %v, want ErrUploadInvalid", err)
	}
}

// TestUploadExtractTarLimit 超限拒绝：tar 字节总量超上限 → ErrUploadTooLarge
//（流式累计即拒；恰好等于上限的合法 tar 不误报）。
func TestUploadExtractTarLimit(t *testing.T) {
	raw := buildTar(t, tarEntry{name: "Dockerfile", body: strings.Repeat("x", 4096)})
	s := newSession(t, int64(len(raw)))
	if _, err := s.ExtractTar(bytes.NewReader(raw)); err != nil {
		t.Fatalf("exact-limit tar rejected: %v", err)
	}

	s2 := newSession(t, int64(len(raw)-1))
	if _, err := s2.ExtractTar(bytes.NewReader(raw)); !errors.Is(err, ErrUploadTooLarge) {
		t.Fatalf("oversize tar err = %v, want ErrUploadTooLarge", err)
	}
}

// TestUploadExtractTarTruncated 截断流（tar 结构不完整）→ ErrUploadInvalid。
func TestUploadExtractTarTruncated(t *testing.T) {
	raw := buildTar(t, tarEntry{name: "Dockerfile", body: strings.Repeat("y", 2048)})
	s := newSession(t, DefaultMaxUploadBytes)
	if _, err := s.ExtractTar(bytes.NewReader(raw[:1024])); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("truncated tar err = %v, want ErrUploadInvalid", err)
	}
}

// TestValidateUploadDockerfile 入口校验：缺省 / 嵌套路径 / 缺失 / 目录 /
// 越界 / 符号链外指逐态。
func TestValidateUploadDockerfile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o750); err != nil {
		t.Fatal(err)
	}

	if got, err := ValidateUploadDockerfile(root, ""); err != nil || got != "Dockerfile" {
		t.Fatalf("default dockerfile = %q err=%v", got, err)
	}
	if got, err := ValidateUploadDockerfile(root, "./deploy/Dockerfile"); err != nil || got != "deploy/Dockerfile" {
		t.Fatalf("nested dockerfile = %q err=%v", got, err)
	}
	if _, err := ValidateUploadDockerfile(root, "missing/Dockerfile"); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("missing dockerfile err = %v, want ErrUploadInvalid", err)
	}
	if _, err := ValidateUploadDockerfile(root, "adir"); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("directory dockerfile err = %v, want ErrUploadInvalid", err)
	}
	if _, err := ValidateUploadDockerfile(root, "../Dockerfile"); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("escaping dockerfile err = %v, want ErrUploadInvalid", err)
	}
	if _, err := ValidateUploadDockerfile(root, `deploy\Dockerfile`); !errors.Is(err, ErrUploadInvalid) {
		t.Fatalf("backslash dockerfile err = %v, want ErrUploadInvalid", err)
	}
	// 符号链外指：root/escape -> root 之外。
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err == nil {
		if _, err := ValidateUploadDockerfile(root, "escape/Dockerfile"); !errors.Is(err, ErrUploadInvalid) {
			t.Fatalf("symlink-escaping dockerfile err = %v, want ErrUploadInvalid", err)
		}
	}
}

// TestCleanupUploadDirGuard 清理的受根约束：直接子目录实体删除；根自身/
// 越界/深层形态/文件/符号链条目一律 no-op。
func TestCleanupUploadDirGuard(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "01CHILD")
	if err := os.MkdirAll(filepath.Join(child, "context"), 0o700); err != nil {
		t.Fatal(err)
	}

	// 越界/根自身/深层形态：no-op（目标仍在）。
	if err := CleanupUploadDir(t.TempDir(), root); err != nil {
		t.Fatalf("outside dir cleanup: %v", err)
	}
	if err := CleanupUploadDir(root, root); err != nil {
		t.Fatalf("root cleanup: %v", err)
	}
	if err := CleanupUploadDir(filepath.Join(child, "context"), root); err != nil {
		t.Fatalf("deep dir cleanup: %v", err)
	}
	if _, err := os.Stat(child); err != nil {
		t.Fatalf("guard no-op violated: %v", err)
	}

	// 直接子目录：删除。
	if err := CleanupUploadDir(child, root); err != nil {
		t.Fatalf("child cleanup: %v", err)
	}
	if _, err := os.Stat(child); !os.IsNotExist(err) {
		t.Fatalf("child still present: %v", err)
	}

	// 相对根（API 与 builder 同装配形态）：仍按直接子目录判定。
	relRoot := t.TempDir()
	relChild := filepath.Join(relRoot, "01RELCHILD")
	if err := os.Mkdir(relChild, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CleanupUploadDir(relChild, relRoot); err != nil {
		t.Fatalf("relative-root cleanup: %v", err)
	}
	if _, err := os.Stat(relChild); !os.IsNotExist(err) {
		t.Fatalf("relative child still present: %v", err)
	}
}

// TestConfigUploadsRootNormalized 配置归一：上传根转绝对路径并恒并入
// ContextRoots（builder 执行侧受管根复核的锚点）；零值 UploadConfig 的
// 上限回落缺省。
func TestConfigUploadsRootNormalized(t *testing.T) {
	root := t.TempDir()
	cfg := Config{UploadsRoot: root, MaxUploadBytes: 0}.Normalize()
	if cfg.MaxUploadBytes != DefaultMaxUploadBytes {
		t.Fatalf("MaxUploadBytes = %d, want default %d", cfg.MaxUploadBytes, DefaultMaxUploadBytes)
	}
	found := false
	for _, r := range cfg.ContextRoots {
		if r == cfg.UploadsRoot {
			found = true
		}
	}
	if !found {
		t.Fatalf("uploads root %q not in ContextRoots %v", cfg.UploadsRoot, cfg.ContextRoots)
	}
	up := cfg.UploadConfig()
	if up.Root != cfg.UploadsRoot || up.MaxBytes != DefaultMaxUploadBytes || up.MaxChunkBytes != MaxUploadChunkBytes {
		t.Fatalf("UploadConfig = %+v, want root=%q max=%d chunk=%d", up, cfg.UploadsRoot, DefaultMaxUploadBytes, MaxUploadChunkBytes)
	}
}
