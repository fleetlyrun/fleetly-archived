package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// TestBootstrapTokenWrittenToFileNotLogged T2.17 验收 + B5（出站字节出口
// 收口）：首启（库内无任何 token）生成 bootstrap admin token 且**只写
// <数据根>/bootstrap-token 文件（0600）**——日志全文不含 flt_ 前缀（systemd
// 形态 journald 不再持久留存明文凭据）；文件存在则不重复生成（幂等）；
// 二次启动不再生成；明文不落库。
func TestBootstrapTokenWrittenToFileNotLogged(t *testing.T) {
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	tokenPath := filepath.Join(dir, "bootstrap-token")

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	// 首启：生成 + 写文件（不打印本体）。
	if err := bootstrapAdminToken(log, tokenPath, st); err != nil {
		t.Fatalf("bootstrapAdminToken: %v", err)
	}
	// 日志全文 grep 不到 flt_ 前缀（B5 验收）。
	if got := strings.Count(buf.String(), "flt_"); got != 0 {
		t.Fatalf("token prefix leaked into logs %d times: %q", got, buf.String())
	}
	if !strings.Contains(buf.String(), tokenPath) {
		t.Fatalf("log must report token file path, got %q", buf.String())
	}
	raw, err := os.ReadFile(tokenPath) //nolint:gosec // G304：读测试临时目录工件
	if err != nil {
		t.Fatalf("read bootstrap token file: %v", err)
	}
	plaintext := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(plaintext, "flt_") {
		t.Fatalf("token file content missing flt_ prefix: %q", plaintext)
	}
	if runtime.GOOS != "windows" { // POSIX 权限位断言（Windows 无 0600 语义）
		if info, serr := os.Stat(tokenPath); serr != nil {
			t.Fatalf("stat token file: %v", serr)
		} else if info.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode = %o, want 0600", info.Mode().Perm())
		}
	}

	// 文件幂等（B5）：库内无 token 但文件在（如手工清库后重启）→ 不重复
	// 签发、不重写文件。用独立空库 + 同一 token 文件路径断言。
	{
		dir2 := t.TempDir()
		st2, err := state.Open(context.Background(), filepath.Join(dir2, "test.db"))
		if err != nil {
			t.Fatalf("state.Open(2): %v", err)
		}
		t.Cleanup(func() { _ = st2.Close() })
		before := buf.String()
		if err := bootstrapAdminToken(log, tokenPath, st2); err != nil {
			t.Fatalf("bootstrapAdminToken (file exists): %v", err)
		}
		after, err := os.ReadFile(tokenPath) //nolint:gosec // G304：读测试临时目录工件
		if err != nil {
			t.Fatalf("re-read token file: %v", err)
		}
		if string(after) != string(raw) {
			t.Fatal("existing token file must not be rewritten")
		}
		if rows2, lerr := st2.ListTokens(context.Background()); lerr != nil || len(rows2) != 0 {
			t.Fatalf("existing token file must short-circuit generation, rows = %d err = %v", len(rows2), lerr)
		}
		if strings.Contains(buf.String()[len(before):], "generated and written to file") {
			t.Fatalf("existing token file must short-circuit generation log: %q", buf.String()[len(before):])
		}
	}

	// 二启（HasAnyToken 恒真）→ 不再生成。
	if err := bootstrapAdminToken(log, tokenPath, st); err != nil {
		t.Fatalf("second bootstrapAdminToken: %v", err)
	}
	if got := strings.Count(buf.String(), "flt_"); got != 0 {
		t.Fatalf("token prefix leaked into logs after restart: %q", buf.String())
	}

	// 明文不落库：库内只有哈希；scope = admin；审计在档。
	rows, err := st.ListTokens(context.Background())
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("token rows = %d, want 1", len(rows))
	}
	if rows[0].Scopes != "admin" {
		t.Fatalf("bootstrap scopes = %q, want admin", rows[0].Scopes)
	}
	if strings.Contains(rows[0].TokenHash, "flt_") {
		t.Fatal("plaintext prefix found in stored hash")
	}
	audits, err := st.RecentAudits(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	var found bool
	for _, a := range audits {
		if a.Action == "token.create" && a.Target == "token:"+rows[0].ID {
			found = true
		}
	}
	if !found {
		t.Fatal("bootstrap token.create audit missing")
	}
}
