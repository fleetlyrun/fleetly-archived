package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPasswordHashRoundTrip argon2id 封装验收（RBAC 设计 §2.1）：正确口令
// 往返成立、错口令拒绝（常量时间比对返回 false 而非 error）、PHC 串形态
// （算法/版本/冻结参数）、salt 随机（同口令两次哈希不同串）。
func TestPasswordHashRoundTrip(t *testing.T) {
	phc, err := HashPassword("s3cret-passw0rd")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=2,p=1$") {
		t.Fatalf("phc prefix = %q, want argon2id v=19 m=65536,t=2,p=1", phc)
	}
	ok, err := VerifyPassword("s3cret-passw0rd", phc)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("correct password must verify")
	}
	ok, err = VerifyPassword("wrong-password", phc)
	if err != nil {
		t.Fatalf("VerifyPassword wrong: %v", err)
	}
	if ok {
		t.Fatal("wrong password must not verify")
	}
	// salt 16B 随机：同口令两次派生串不同（PHC 串可区分）。
	phc2, err := HashPassword("s3cret-passw0rd")
	if err != nil {
		t.Fatalf("HashPassword second: %v", err)
	}
	if phc == phc2 {
		t.Fatal("two hashes of the same password must differ (random salt)")
	}
	if !strings.Contains(phc2, "$argon2id$") {
		t.Fatal("second hash must be argon2id PHC form")
	}
	// PHC 串内的 hash 段与 salt 段长度固定（keyLen=32 → 43 b64 字符；
	// saltLen=16 → 22 b64 字符，RawStd 无填充）。
	parts := strings.Split(phc, "$")
	if len(parts) != 6 {
		t.Fatalf("phc field count = %d, want 6", len(parts))
	}
	if len(parts[4]) != 22 || len(parts[5]) != 43 {
		t.Fatalf("salt/hash b64 lengths = %d/%d, want 22/43", len(parts[4]), len(parts[5]))
	}
}

// TestPasswordHashTamperedPHC 篡改 PHC 串各段 → ErrPasswordHashInvalid
// （fail-closed，不做宽松兜底）。
func TestPasswordHashTamperedPHC(t *testing.T) {
	phc, err := HashPassword("pw")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	cases := map[string]string{
		"algorithm swapped": strings.Replace(phc, "argon2id", "argon2i", 1),
		"version corrupted": strings.Replace(phc, "v=19", "v=16", 1),
		"params corrupted":  strings.Replace(phc, "m=65536", "m=abc", 1),
		"hash truncated":    phc[:len(phc)-3],
		"field dropped":     strings.TrimPrefix(phc, "$argon2id$"),
		"not a phc":         "plain-text-not-a-hash",
		"empty":             "",
	}
	for name, tampered := range cases {
		if _, err := VerifyPassword("pw", tampered); !errors.Is(err, ErrPasswordHashInvalid) {
			t.Errorf("%s: err = %v, want ErrPasswordHashInvalid", name, err)
		}
	}
}

// TestCreateUserEmailNormalizationAndConflict（RBAC 设计 §2.1）：email 小写
// 归一落库、大小写变体查得到、归一后冲突返回 ErrEmailTaken；display_name
// 缺省取 email 本地部分；首用户强制平台管理员（设计 §2.1 首注册者规则），
// 后续用户缺省非管理员。
func TestCreateUserEmailNormalizationAndConflict(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	first, err := st.CreateUser(ctx, UserWrite{Email: "Alice@Example.COM", Password: "pw-1"})
	if err != nil {
		t.Fatalf("CreateUser first: %v", err)
	}
	if first.Email != "alice@example.com" {
		t.Fatalf("stored email = %q, want lowercase normalized", first.Email)
	}
	if !first.IsPlatformAdmin {
		t.Fatal("first registered user must be platform admin (design §2.1)")
	}
	if first.DisplayName != "alice" {
		t.Fatalf("default display name = %q, want email local part", first.DisplayName)
	}

	// 大小写变体冲突 → ErrEmailTaken（归一后同串）。
	if _, err := st.CreateUser(ctx, UserWrite{Email: "ALICE@example.com", Password: "x"}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email err = %v, want ErrEmailTaken", err)
	}

	// 首用户规则覆盖显式 false；第二个用户显式 true 才是管理员。
	second, err := st.CreateUser(ctx, UserWrite{Email: "bob@example.com", Password: "pw-2", IsPlatformAdmin: false})
	if err != nil {
		t.Fatalf("CreateUser second: %v", err)
	}
	if second.IsPlatformAdmin {
		t.Fatal("second user must not be platform admin by default")
	}

	// GetUser / GetUserByEmail（任意大小写）/ ListUsers。
	got, err := st.GetUser(ctx, first.ID)
	if err != nil || got.Email != "alice@example.com" {
		t.Fatalf("GetUser: %v (%+v)", err, got)
	}
	byEmail, err := st.GetUserByEmail(ctx, "  BOB@Example.com ")
	if err != nil || byEmail.ID != second.ID {
		t.Fatalf("GetUserByEmail: %v (%+v)", err, byEmail)
	}
	if _, err := st.GetUserByEmail(ctx, "missing@example.com"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("GetUserByEmail missing err = %v, want ErrUserNotFound", err)
	}
	users, err := st.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("ListUsers: %v (n=%d)", err, len(users))
	}
	if users[0].ID != first.ID || users[1].ID != second.ID {
		t.Fatal("ListUsers must be created_at ascending")
	}
	if users[0].CreatedAt.IsZero() {
		t.Fatal("CreatedAt must be set")
	}

	// 畸形 email 拒绝（无 @）；空口令拒绝。
	if _, err := st.CreateUser(ctx, UserWrite{Email: "not-an-email", Password: "x"}); err == nil {
		t.Fatal("email without @ must be rejected")
	}
	if _, err := st.CreateUser(ctx, UserWrite{Email: "x@y.com", Password: "  "}); err == nil {
		t.Fatal("empty password must be rejected")
	}
}

// TestAuthenticateUser（RBAC 设计 §2.1）：正确凭据通过；口令错/email 不存在
// /账号禁用统一 ErrInvalidCredentials（不泄漏存在性与账号状态）；口令重置
// 后旧口令失效新口令生效。
func TestAuthenticateUser(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, UserWrite{Email: "dev@example.com", Password: "right-pass"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if _, err := st.AuthenticateUser(ctx, "dev@example.com", "right-pass"); err != nil {
		t.Fatalf("AuthenticateUser: %v", err)
	}
	// 大小写形态的 email 也能登录（归一在认证通道同样生效）。
	if _, err := st.AuthenticateUser(ctx, "DEV@Example.com", "right-pass"); err != nil {
		t.Fatalf("AuthenticateUser mixed case: %v", err)
	}
	for name, tc := range map[string]struct{ email, pass string }{
		"wrong password": {"dev@example.com", "wrong-pass"},
		"unknown email":  {"nobody@example.com", "right-pass"},
		"malformed mail": {"not-an-email", "right-pass"},
	} {
		if _, err := st.AuthenticateUser(ctx, tc.email, tc.pass); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: err = %v, want ErrInvalidCredentials", name, err)
		}
	}

	// 禁用 → 拒认同码；解禁 → 恢复。
	if err := st.DisableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if _, err := st.AuthenticateUser(ctx, "dev@example.com", "right-pass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled user err = %v, want ErrInvalidCredentials", err)
	}
	if err := st.EnableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}
	if _, err := st.AuthenticateUser(ctx, "dev@example.com", "right-pass"); err != nil {
		t.Fatalf("re-enabled user: %v", err)
	}

	// 口令重置：旧口令失效、新口令生效。
	if err := st.ResetPassword(ctx, u.ID, "new-pass", "", ""); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if _, err := st.AuthenticateUser(ctx, "dev@example.com", "right-pass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password must fail after reset")
	}
	if _, err := st.AuthenticateUser(ctx, "dev@example.com", "new-pass"); err != nil {
		t.Fatalf("new password after reset: %v", err)
	}
	if err := st.ResetPassword(ctx, "01MISSING000000000000000000", "x", "", ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("reset missing user err = %v, want ErrUserNotFound", err)
	}
}

// TestUserAdminFlagAndDisableIdempotence：SetPlatformAdmin 授/撤幂等面、
// 目标不存在 ErrUserNotFound；Disable/Enable 幂等面同型；禁用时间戳落定；
// 生命周期动作与审计同事务落档（设计 §6 动作注册表）。首用户在册即为
// 平台管理员（CreateUser 首用户规则）——授予/撤销/口令重置的审计断言用
// 第二个用户驱动。
func TestUserAdminFlagAndDisableIdempotence(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, UserWrite{Email: "op@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	v, err := st.CreateUser(ctx, UserWrite{Email: "second@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser second: %v", err)
	}
	if err := st.SetPlatformAdmin(ctx, v.ID, true, "", ""); err != nil {
		t.Fatalf("SetPlatformAdmin grant: %v", err)
	}
	if err := st.SetPlatformAdmin(ctx, v.ID, true, "", ""); err != nil {
		t.Fatalf("grant idempotent: %v", err)
	}
	got, _ := st.GetUser(ctx, v.ID)
	if !got.IsPlatformAdmin {
		t.Fatal("platform admin flag must be set")
	}
	if err := st.SetPlatformAdmin(ctx, v.ID, false, "", ""); err != nil {
		t.Fatalf("SetPlatformAdmin revoke: %v", err)
	}
	got, _ = st.GetUser(ctx, v.ID)
	if got.IsPlatformAdmin {
		t.Fatal("platform admin flag must be cleared")
	}
	if err := st.SetPlatformAdmin(ctx, "01MISSING000000000000000000", true, "", ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("grant missing err = %v, want ErrUserNotFound", err)
	}

	if err := st.DisableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if err := st.DisableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("disable idempotent: %v", err)
	}
	got, _ = st.GetUser(ctx, u.ID)
	if got.DisabledAt.IsZero() {
		t.Fatal("disabled_at must be set")
	}
	if err := st.EnableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}
	if err := st.EnableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("enable idempotent: %v", err)
	}
	got, _ = st.GetUser(ctx, u.ID)
	if !got.DisabledAt.IsZero() {
		t.Fatal("disabled_at must be cleared after enable")
	}
	if err := st.DisableUser(ctx, "01MISSING000000000000000000", "", ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("disable missing err = %v, want ErrUserNotFound", err)
	}

	if err := st.ResetPassword(ctx, v.ID, "reset-pw", "", ""); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if _, err := st.AuthenticateUser(ctx, "second@example.com", "reset-pw"); err != nil {
		t.Fatalf("authenticate after reset: %v", err)
	}

	// 生命周期动作与审计同事务落档（设计 §6 动作注册表；按 target 用户
	// 分组核对——首用户的授予路径被首用户规则短路为幂等，不产生审计）。
	audits, err := st.RecentAudits(ctx, 50)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	seen := map[string]map[string]bool{"u": {}, "v": {}}
	for _, a := range audits {
		switch a.Target {
		case "user:" + u.ID:
			seen["u"][a.Action] = true
		case "user:" + v.ID:
			seen["v"][a.Action] = true
		}
	}
	for _, action := range []string{"user.created", "user.disabled", "user.enabled"} {
		if !seen["u"][action] {
			t.Errorf("audit action %s missing for user %s", action, u.ID)
		}
	}
	for _, action := range []string{
		"user.created", "user.platform_admin_granted", "user.platform_admin_revoked", "user.password_reset",
	} {
		if !seen["v"][action] {
			t.Errorf("audit action %s missing for user %s", action, v.ID)
		}
	}
}

// TestDisableUserRevokesSessionsAndPATs（RBAC 设计 §2.1/§10）：禁用 = 会话
// 全部删除（同事务联动）+ 其 PAT 拒认（ErrTokenInvalid 同码——不泄漏存在
// 性）；解禁后 PAT 恢复可用，已删会话不复活。
func TestDisableUserRevokesSessionsAndPATs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, UserWrite{Email: "victim@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	sess, plaintext, err := st.CreateSession(ctx, u.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, err := st.AuthenticateSession(ctx, plaintext); err != nil {
		t.Fatalf("session auth before disable: %v", err)
	}
	pat, err := st.CreateToken(ctx, TokenWrite{Hash: HashToken("flt_user_pat"), Name: "pat", Scopes: "read,deploy", UserID: u.ID})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if got, err := st.AuthenticateToken(ctx, "flt_user_pat"); err != nil || got.UserID != u.ID {
		t.Fatalf("PAT auth before disable: %v (%+v)", err, got)
	}

	if err := st.DisableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	// PAT 拒认：ErrTokenInvalid 同码（非 ErrTokenRevoked——不泄漏存在性）。
	if _, err := st.AuthenticateToken(ctx, "flt_user_pat"); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("disabled user PAT err = %v, want ErrTokenInvalid", err)
	}
	// 会话行已删（同事务联动）：认证拒 + 行不存在。
	if _, err := st.AuthenticateSession(ctx, plaintext); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("disabled user session err = %v, want ErrSessionInvalid", err)
	}
	var n int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM sessions WHERE id = ?`, sess.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("session rows after disable = %d err=%v, want 0", n, err)
	}

	// 解禁：PAT 恢复（disabled_at 是唯一拒认依据），会话不复活。
	if err := st.EnableUser(ctx, u.ID, "", ""); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}
	if got, err := st.AuthenticateToken(ctx, "flt_user_pat"); err != nil || got.ID != pat.ID {
		t.Fatalf("PAT auth after re-enable: %v", err)
	}
	if _, err := st.AuthenticateSession(ctx, plaintext); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("revoked sessions must not resurrect after enable")
	}
}

// TestMigration00018RBACSchema（迁移 00018 验收 + 00019 收紧后的终态口径）：
// 七新表存在、加列就位、设计 §8 清单索引在册。
//
// v0.3 W2-S3 00019 表重建后的终态口径（rbac-teams §8 实现切分）：apps/
// db_instances 的 project_id/team_id 收紧为 NOT NULL（切分设计：00018 可空
// → 00019 收紧）；UNIQUE(project_id,name) 由表约束承载（00018 的具名唯一
// 索引 idx_*_project_name 随重建收敛为 sqlite_autoindex），全局 UNIQUE(name)
// 退役（D-W0-4 二修）。
func TestMigration00018RBACSchema(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	for _, table := range []string{
		"users", "sessions", "teams", "team_members", "team_invites",
		"projects", "project_members",
	} {
		var name string
		if err := st.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}

	// 加列清单（列名 → 携表）+ 00019 收紧后的 NOT NULL 终态：
	//   apps/db_instances 归属列 = NOT NULL（notnull = 1）；
	//   tokens/git_keys 用户化列保持可空（语义 NULL：机具令牌/不绑定/存量）。
	for _, tc := range []struct {
		table, col  string
		wantNotNull int
	}{
		{"apps", "project_id", 1}, {"apps", "team_id", 1},
		{"db_instances", "project_id", 1}, {"db_instances", "team_id", 1},
		{"tokens", "user_id", 0}, {"tokens", "project_id", 0},
		{"git_keys", "user_id", 0},
	} {
		rows, err := st.db.QueryContext(ctx, `PRAGMA table_info(`+tc.table+`)`)
		if err != nil {
			t.Fatalf("table_info %s: %v", tc.table, err)
		}
		found := false
		notNull := -1
		for rows.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dfltValue any
			var pk int
			if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
				_ = rows.Close()
				t.Fatalf("scan table_info %s: %v", tc.table, err)
			}
			if name == tc.col {
				found = true
				notNull = notnull
			}
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate table_info %s: %v", tc.table, err)
		}
		if !found {
			t.Fatalf("column %s.%s missing", tc.table, tc.col)
		}
		if notNull != tc.wantNotNull {
			t.Fatalf("column %s.%s notnull = %d, want %d (00019 tightening)", tc.table, tc.col, notNull, tc.wantNotNull)
		}
	}

	// 索引清单（设计 §8）：PRAGMA index_list 逐表收集，核对普通与复合唯一索引。
	indexesOf := func(table string) map[string]bool {
		rows, err := st.db.QueryContext(ctx, `PRAGMA index_list(`+table+`)`)
		if err != nil {
			t.Fatalf("index_list %s: %v", table, err)
		}
		defer func() { _ = rows.Close() }()
		out := map[string]bool{}
		for rows.Next() {
			var seq int
			var name string
			var unique, origin, partial any
			if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
				t.Fatalf("scan index_list %s: %v", table, err)
			}
			out[name] = true
		}
		return out
	}
	for table, want := range map[string][]string{
		"apps":            {"idx_apps_project", "idx_apps_team"},
		"db_instances":    {"idx_db_instances_project", "idx_db_instances_team"},
		"tokens":          {"idx_tokens_user"},
		"team_members":    {"idx_team_members_team", "idx_team_members_user"},
		"projects":        {"idx_projects_team"},
		"project_members": {"idx_project_members_project"},
		"sessions":        {"idx_sessions_user", "idx_sessions_expires_at"},
	} {
		got := indexesOf(table)
		for _, idx := range want {
			if !got[idx] {
				t.Errorf("index %s on %s missing (have %v)", idx, table, got)
			}
		}
	}

	// UNIQUE(project_id, name) 以表约束在册（00019 重建后 = sqlite_autoindex
	// 具名消失）；列序核对改走 autoindex（按 sqlite_master sql 断言约束在表
	// 定义内，免疫 autoindex 命名细节）。
	var appsDDL string
	if err := st.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='apps'`).Scan(&appsDDL); err != nil {
		t.Fatalf("read apps ddl: %v", err)
	}
	if !strings.Contains(appsDDL, "UNIQUE (project_id, name)") {
		t.Fatalf("apps table constraint UNIQUE(project_id, name) missing in ddl: %s", appsDDL)
	}
	if strings.Contains(appsDDL, `name TEXT NOT NULL UNIQUE`) {
		t.Fatalf("apps.name must not carry a global UNIQUE constraint (D-W0-4): %s", appsDDL)
	}
	var dbDDL string
	if err := st.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='db_instances'`).Scan(&dbDDL); err != nil {
		t.Fatalf("read db_instances ddl: %v", err)
	}
	if !strings.Contains(dbDDL, "UNIQUE (project_id, name)") {
		t.Fatalf("db_instances table constraint UNIQUE(project_id, name) missing in ddl: %s", dbDDL)
	}
	if strings.Contains(dbDDL, `name                  TEXT NOT NULL UNIQUE`) {
		t.Fatalf("db_instances.name must not carry a global UNIQUE constraint (D-W0-4): %s", dbDDL)
	}
}
