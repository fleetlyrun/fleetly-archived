package state

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRegisterUserFirstUserFullProvision（RBAC 设计 §2.1/§2.3/§3.1，本票核
// 心验收）：无用户窗口恒开；首用户 is_platform_admin=1；同事务落位个人
// Team（owner 一人，slug = email 本地部分归一）+ 默认 Project `default`；
// bootstrap token 同事务自动吊销；事件 user.registered/team.created/
// project.created 与审计动作全数落档。
func TestRegisterUserFirstUserFullProvision(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 用 S1 原语先落一枚 bootstrap 形态 token（name 常量 + 机具令牌 +
	// 在册 = 精确识别面），以及一枚不应被误伤的普通机具 token。
	bootstrap, err := st.CreateToken(ctx, TokenWrite{
		Hash: HashToken("flt_bootstrap_plain"), Name: BootstrapTokenName, Scopes: "admin",
	})
	if err != nil {
		t.Fatalf("seed bootstrap token: %v", err)
	}
	if _, err := st.CreateToken(ctx, TokenWrite{
		Hash: HashToken("flt_machine_plain"), Name: "CI machine token", Scopes: "read",
	}); err != nil {
		t.Fatalf("seed machine token: %v", err)
	}

	rr, err := st.RegisterUser(ctx, RegisterWrite{Email: "Founder@Example.COM", Password: "pw-founder"})
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	if rr.User.Email != "founder@example.com" {
		t.Fatalf("email = %q, want lowercase normalized", rr.User.Email)
	}
	if !rr.User.IsPlatformAdmin {
		t.Fatal("first registered user must be platform admin (design §2.1)")
	}
	if rr.User.DisplayName != "founder" {
		t.Fatalf("display name = %q, want email local part", rr.User.DisplayName)
	}
	if rr.Team.Slug != "founder" || rr.Team.CreatedBy != rr.User.ID {
		t.Fatalf("personal team = %+v (slug should derive from the email local part)", rr.Team)
	}
	if rr.TeamRole != TeamRoleOwner {
		t.Fatalf("team role = %q, want owner", rr.TeamRole)
	}
	if rr.Project.Slug != "default" || rr.Project.TeamID != rr.Team.ID {
		t.Fatalf("default project = %+v", rr.Project)
	}

	// bootstrap token 同事务吊销；普通机具 token 不受牵连。
	if _, err := st.AuthenticateToken(ctx, "flt_bootstrap_plain"); !errors.Is(err, ErrTokenRevoked) {
		t.Fatalf("bootstrap token after registration err = %v, want ErrTokenRevoked", err)
	}
	if _, err := st.AuthenticateToken(ctx, "flt_machine_plain"); err != nil {
		t.Fatalf("unrelated machine token must survive: %v", err)
	}
	var revoked int64
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM tokens WHERE id = ? AND revoked_at IS NOT NULL`, bootstrap.ID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("bootstrap row revoked count = %d err=%v, want 1 (design §2.3: no standing backdoor)", revoked, err)
	}

	// 审计动作面：user.created / team.created / team.member_added /
	// project.created / token.revoke 全数落档（与业务写同事务）。
	audits, err := st.RecentAudits(ctx, 50)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	seen := map[string]bool{}
	for _, a := range audits {
		seen[a.Action] = true
		if a.Action == "token.revoke" && a.Actor != "system" {
			t.Fatalf("bootstrap revoke audit actor = %q, want system", a.Actor)
		}
	}
	for _, action := range []string{"user.created", "team.created", "team.member_added", "project.created", "token.revoke"} {
		if !seen[action] {
			t.Errorf("audit action %s missing after first registration", action)
		}
	}

	// 事件面（设计 §6 注册态三类，metadata-only）：三条同事务落 events 表。
	var names []string
	rows, err := st.db.QueryContext(ctx, `SELECT name FROM events WHERE name IN ('user.registered','team.created','project.created') ORDER BY seq ASC`)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate events: %v", err)
	}
	// 发出顺序随注册事务内的落位序：team → project → user.registered。
	if len(names) != 3 || names[0] != "team.created" || names[1] != "project.created" || names[2] != "user.registered" {
		t.Fatalf("registration events = %v, want [team.created project.created user.registered]", names)
	}
}

// TestRegisterUserWindowRules（RBAC 设计 §2.1/§10）：users 非空后由
// auth.registration 管辖——缺省 closed 拒绝；open 放行；显式 closed 再拒。
// email 冲突 ErrEmailTaken；个人队 slug 冲突加 2 起序号。
func TestRegisterUserWindowRules(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if _, err := st.RegisterUser(ctx, RegisterWrite{Email: "first@example.com", Password: "pw"}); err != nil {
		t.Fatalf("first registration (empty users table) must always be open: %v", err)
	}

	// 缺省 closed：users 非空后注册拒绝。
	if _, err := st.RegisterUser(ctx, RegisterWrite{Email: "second@example.com", Password: "pw"}); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("default-window registration err = %v, want ErrRegistrationClosed", err)
	}

	// 显式 open → 放行；email 大小写变体冲突 → ErrEmailTaken。
	if err := st.SaveRegistration(ctx, AuthRegistrationOpen, AuthSaveOptions{Actor: "user:x"}); err != nil {
		t.Fatalf("SaveRegistration open: %v", err)
	}
	second, err := st.RegisterUser(ctx, RegisterWrite{Email: "second@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("open-window registration: %v", err)
	}
	if second.User.IsPlatformAdmin {
		t.Fatal("second registered user must not be platform admin")
	}
	if _, err := st.RegisterUser(ctx, RegisterWrite{Email: "SECOND@Example.com", Password: "pw"}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email err = %v, want ErrEmailTaken", err)
	}
	// 第二用户同样获得个人队 + 默认项目（R3：注册默认建队）。
	if second.Team.Slug != "second" || second.Project.Slug != "default" {
		t.Fatalf("second user provisioning = team:%s project:%s", second.Team.Slug, second.Project.Slug)
	}

	// 显式 closed → 再拒。
	if err := st.SaveRegistration(ctx, AuthRegistrationClosed, AuthSaveOptions{Actor: "user:x"}); err != nil {
		t.Fatalf("SaveRegistration closed: %v", err)
	}
	if _, err := st.RegisterUser(ctx, RegisterWrite{Email: "third@example.com", Password: "pw"}); !errors.Is(err, ErrRegistrationClosed) {
		t.Fatalf("closed-window registration err = %v, want ErrRegistrationClosed", err)
	}
	// 空口令拒绝。
	if _, err := st.RegisterUser(ctx, RegisterWrite{Email: "x@example.com", Password: " "}); err == nil {
		t.Fatal("empty password must be rejected")
	}
}

// TestRegisterUserSlugCollisionSuffix（RBAC 设计 §3.1：slug 归一冲突加 2
// 起序号）：本地部分只保留 [a-z0-9]（剥离分隔符压成单词）；短/空本地部分
// 回落 `user`；冲突序号使 slug 保持 32 位上界。
func TestRegisterUserSlugCollisionSuffix(t *testing.T) {
	if got := normalizeTeamSlug("Alice.Bob-Clark_99"); got != "alicebobclark99" {
		t.Fatalf("normalizeTeamSlug = %q, want alicebobclark99", got)
	}
	if got := normalizeTeamSlug("a"); got != "user" {
		t.Fatalf("short local part = %q, want user fallback", got)
	}
	if got := normalizeTeamSlug(strings.Repeat("x", 40)); len(got) != 32 {
		t.Fatalf("long local part length = %d, want 32", len(got))
	}
	// 序号：两个本地部分同为 "ops" 的注册 → ops 与 ops2；序号截断保持 32 位。
	if got := normalizeTeamSlug("o.p.s"); got != "ops" {
		t.Fatalf("separated local part = %q, want ops", got)
	}

	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.RegisterUser(ctx, RegisterWrite{Email: "ops@example.com", Password: "pw"}); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := st.SaveRegistration(ctx, AuthRegistrationOpen, AuthSaveOptions{Actor: "user:x"}); err != nil {
		t.Fatalf("open window: %v", err)
	}
	second, err := st.RegisterUser(ctx, RegisterWrite{Email: "o.p.s@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("colliding registration: %v", err)
	}
	if second.Team.Slug != "ops2" {
		t.Fatalf("colliding slug = %q, want ops2 (suffix from 2)", second.Team.Slug)
	}
}

// TestRegisterUserWithInviteToken（v0.3 W3-S4，RBAC 设计 §3.1「未注册→
// 注册即自动 accept」）：关窗状态下带有效邀请 token 的注册豁免注册窗、
// 注册成功即同事务以受邀角色入队；审计（team.invite_accepted）与事件
//（invite.accepted）经 ConsumeInvite 同一写面恰好单落；无效 token（查无/
// 已消费/已吊销/已过期）在任何写入前整笔回滚 ErrInviteInvalid——用户行
// 不残留，同一 email 仍可走无 token 原路径（开窗后）注册。
func TestRegisterUserWithInviteToken(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// 邀请产生面（不依赖注册窗）：owner + 团队 + developer 邀请。
	owner, err := st.CreateUser(ctx, UserWrite{Email: "owner@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	team, err := st.CreateTeam(ctx, TeamWrite{Slug: "acme", Name: "Acme", CreatedBy: owner.ID})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if _, err := st.AddMember(ctx, team.ID, owner.ID, TeamRoleOwner, "", ""); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}
	_, token, err := st.CreateInvite(ctx, InviteWrite{
		TeamID: team.ID, Email: "newbie@example.com", Role: TeamRoleDeveloper, ActorUserID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// 注册窗口缺省 closed（users 非空）+ 有效 token → 注册成功（豁免注册
	// 窗）且即入队：个人队 owner + acme developer 双成员关系。
	rr, err := st.RegisterUser(ctx, RegisterWrite{
		Email: "Newbie@Example.COM", Password: "pw-newbie-1", InviteToken: token,
	})
	if err != nil {
		t.Fatalf("invite registration with closed window: %v", err)
	}
	if rr.User.IsPlatformAdmin {
		t.Fatal("invited registrant must not be platform admin")
	}
	if rr.TeamRole != TeamRoleOwner || rr.Project.Slug != "default" {
		t.Fatalf("personal provisioning = role:%s project:%s, want owner/default", rr.TeamRole, rr.Project.Slug)
	}
	ms, err := st.ListUserMemberships(ctx, rr.User.ID)
	if err != nil || len(ms) != 2 {
		t.Fatalf("memberships after invite registration = %v (%v), want 2", ms, err)
	}
	// 排序键是 (created_at, team_id)——同事务两行在时钟粒度内可能并列，
	// 按 team_id 定位断言，不依赖行序。
	var personal, invited *TeamMember
	for i := range ms {
		switch ms[i].TeamID {
		case rr.Team.ID:
			personal = &ms[i]
		case team.ID:
			invited = &ms[i]
		}
	}
	if personal == nil || personal.Role != TeamRoleOwner {
		t.Fatalf("personal membership missing/ wrong role: %v", ms)
	}
	if invited == nil || invited.Role != TeamRoleDeveloper {
		t.Fatalf("invited membership = %v, want developer on acme (team %s)", ms, team.ID)
	}
	// 邀请行已消费（accepted_at 置位）。
	invites, err := st.ListInvites(ctx, team.ID)
	if err != nil || len(invites) != 1 || invites[0].AcceptedAt.IsZero() {
		t.Fatalf("invite after registration = %+v err=%v, want accepted_at set", invites, err)
	}

	// 审计/事件不双落：invite.accepted 事件恰 1 条；team.invite_accepted
	// 审计恰 1 条；via invite:<id> 的 member_added 审计恰 1 条（个人队的
	// member_added 不带 via 邀请，不计入）。
	var inviteAcceptedEvents int64
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM events WHERE name = 'invite.accepted'`).Scan(&inviteAcceptedEvents); err != nil {
		t.Fatalf("count invite.accepted events: %v", err)
	}
	if inviteAcceptedEvents != 1 {
		t.Fatalf("invite.accepted events = %d, want exactly 1 (ConsumeInvite shared write face)", inviteAcceptedEvents)
	}
	audits, err := st.RecentAudits(ctx, 50)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	acceptedAudits, viaInviteAdds := 0, 0
	for _, a := range audits {
		if a.Action == "team.invite_accepted" {
			acceptedAudits++
		}
		if a.Action == "team.member_added" && strings.Contains(a.DiffSummary, "invite:") {
			viaInviteAdds++
		}
	}
	if acceptedAudits != 1 || viaInviteAdds != 1 {
		t.Fatalf("invite audits = invite_accepted:%d member_added(via invite):%d, want 1/1", acceptedAudits, viaInviteAdds)
	}

	// token 复用（已消费）→ 注册拒绝，ErrInviteInvalid。
	if _, err := st.RegisterUser(ctx, RegisterWrite{
		Email: "reuse@example.com", Password: "pw", InviteToken: token,
	}); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("registration with consumed invite err = %v, want ErrInviteInvalid", err)
	}

	// 查无此 token → 同码拒绝（不泄漏存在性）。
	if _, err := st.RegisterUser(ctx, RegisterWrite{
		Email: "unknown@example.com", Password: "pw", InviteToken: "bogus-token",
	}); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("registration with unknown invite err = %v, want ErrInviteInvalid", err)
	}

	// 已吊销邀请 → 拒绝。
	_, revToken, err := st.CreateInvite(ctx, InviteWrite{
		TeamID: team.ID, Email: "revoked@example.com", Role: TeamRoleViewer, ActorUserID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateInvite revocation fixture: %v", err)
	}
	invites, err = st.ListInvites(ctx, team.ID)
	if err != nil {
		t.Fatalf("ListInvites: %v", err)
	}
	var revID string
	for _, inv := range invites {
		if inv.AcceptedAt.IsZero() {
			revID = inv.ID
		}
	}
	if err := st.RevokeInvite(ctx, revID, owner.ID, ""); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if _, err := st.RegisterUser(ctx, RegisterWrite{
		Email: "revoked@example.com", Password: "pw", InviteToken: revToken,
	}); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("registration with revoked invite err = %v, want ErrInviteInvalid", err)
	}

	// 已过期邀请 → 拒绝（expires_at 回拨构造过期存量——store 层无时钟注入）。
	_, expToken, err := st.CreateInvite(ctx, InviteWrite{
		TeamID: team.ID, Email: "expired@example.com", Role: TeamRoleViewer, ActorUserID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateInvite expiry fixture: %v", err)
	}
	invites, err = st.ListInvites(ctx, team.ID)
	if err != nil {
		t.Fatalf("ListInvites 2: %v", err)
	}
	var expID string
	for _, inv := range invites {
		if inv.AcceptedAt.IsZero() && inv.ID != revID {
			expID = inv.ID
		}
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE team_invites SET expires_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).UnixNano(), expID); err != nil {
		t.Fatalf("backdate expires_at: %v", err)
	}
	if _, err := st.RegisterUser(ctx, RegisterWrite{
		Email: "expired@example.com", Password: "pw", InviteToken: expToken,
	}); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("registration with expired invite err = %v, want ErrInviteInvalid", err)
	}

	// 负例零副作用：四个失败注册均未建用户行（行数 = owner + 新bie）。
	var users int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&users); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 2 {
		t.Fatalf("user rows after failed invite registrations = %d, want 2 (failed writes must roll back)", users)
	}

	// 无 token 原路径回归：开窗后同 email（某负例残留邮箱）注册照常成功。
	if err := st.SaveRegistration(ctx, AuthRegistrationOpen, AuthSaveOptions{Actor: "user:x"}); err != nil {
		t.Fatalf("open window: %v", err)
	}
	plain, err := st.RegisterUser(ctx, RegisterWrite{Email: "unknown@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("plain registration (no token) after opening window: %v", err)
	}
	if len(plain.Team.Slug) == 0 || plain.Project.Slug != "default" {
		t.Fatalf("plain registration provisioning = %+v", plain)
	}
}

// TestResetPasswordRevokesSessions（本票裁决：重置即全端下线，设计 §2.1）：
// 口令重置与该用户全部会话吊销同事务生效；他用户会话不动；审计带
// sessions_revoked 计数。
func TestResetPasswordRevokesSessions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, UserWrite{Email: "victim@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	other, err := st.CreateUser(ctx, UserWrite{Email: "other@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser second: %v", err)
	}
	sess1, plain1, err := st.CreateSession(ctx, u.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	if _, _, err := st.CreateSession(ctx, u.ID, time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}
	_, otherPlain, err := st.CreateSession(ctx, other.ID, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession other: %v", err)
	}

	if err := st.ResetPassword(ctx, u.ID, "new-pass", "", ""); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	// 该用户全部会话失效（含第二会话——删行）。
	if _, err := st.AuthenticateSession(ctx, plain1); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("session after reset err = %v, want ErrSessionInvalid", err)
	}
	var n int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM sessions WHERE user_id = ?`, sess1.UserID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("session rows after reset = %d err=%v, want 0", n, err)
	}
	// 他用户会话不动。
	if _, err := st.AuthenticateSession(ctx, otherPlain); err != nil {
		t.Fatalf("other user session must survive: %v", err)
	}
	// 审计带吊销计数。
	audits, err := st.RecentAudits(ctx, 10)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "user.password_reset" && a.Target == "user:"+u.ID {
			found = true
			if !strings.Contains(a.DiffSummary, "2") {
				t.Fatalf("password_reset diff = %q, want sessions_revoked=2", a.DiffSummary)
			}
		}
	}
	if !found {
		t.Fatal("user.password_reset audit row missing")
	}
}

// TestHasAnyUser（注册窗口谓词）：空库 false、建用户后 true。
func TestHasAnyUser(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if has, err := st.HasAnyUser(ctx); err != nil || has {
		t.Fatalf("HasAnyUser on empty store = %v err=%v, want false", has, err)
	}
	if _, err := st.CreateUser(ctx, UserWrite{Email: "u@example.com", Password: "pw"}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if has, err := st.HasAnyUser(ctx); err != nil || !has {
		t.Fatalf("HasAnyUser after create = %v err=%v, want true", has, err)
	}
}

// TestAuthRegistrationSettings（platform_settings auth.registration，
// logsettings 同型验收）：缺省 closed + Set=false；open/closed 往返；非法值
// 保存拒绝；审计 auth.registration_changed 落档。
func TestAuthRegistrationSettings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	settings, err := st.LoadAuthSettings(ctx)
	if err != nil {
		t.Fatalf("LoadAuthSettings empty: %v", err)
	}
	if settings.Registration != AuthRegistrationClosed || settings.Set {
		t.Fatalf("default settings = %+v, want closed + unset", settings)
	}
	if err := ValidateRegistrationMode("maybe"); err == nil {
		t.Fatal("invalid registration value must be rejected")
	}
	if err := st.SaveRegistration(ctx, "maybe", AuthSaveOptions{}); err == nil {
		t.Fatal("saving invalid value must fail")
	}
	if err := st.SaveRegistration(ctx, AuthRegistrationOpen, AuthSaveOptions{Actor: "user:op"}); err != nil {
		t.Fatalf("SaveRegistration open: %v", err)
	}
	settings, err = st.LoadAuthSettings(ctx)
	if err != nil || settings.Registration != AuthRegistrationOpen || !settings.Set {
		t.Fatalf("settings after save = %+v err=%v, want open + set", settings, err)
	}
	audits, err := st.RecentAudits(ctx, 5)
	if err != nil {
		t.Fatalf("RecentAudits: %v", err)
	}
	if len(audits) == 0 || audits[0].Action != "auth.registration_changed" || audits[0].Actor != "user:op" {
		t.Fatalf("top audit = %+v, want auth.registration_changed by user:op", audits[0])
	}
}

// TestListUserMemberships（Me 投影原语）：按用户取全部成员关系，跨团队
// 升序；无成员返回空。
func TestListUserMemberships(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u, err := st.CreateUser(ctx, UserWrite{Email: "me@example.com", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	t1, err := st.CreateTeam(ctx, TeamWrite{Slug: "alpha", Name: "Alpha", CreatedBy: u.ID})
	if err != nil {
		t.Fatalf("CreateTeam 1: %v", err)
	}
	t2, err := st.CreateTeam(ctx, TeamWrite{Slug: "beta", Name: "Beta", CreatedBy: u.ID})
	if err != nil {
		t.Fatalf("CreateTeam 2: %v", err)
	}
	if _, err := st.AddMember(ctx, t1.ID, u.ID, TeamRoleOwner, "", ""); err != nil {
		t.Fatalf("AddMember 1: %v", err)
	}
	if _, err := st.AddMember(ctx, t2.ID, u.ID, TeamRoleAdmin, "", ""); err != nil {
		t.Fatalf("AddMember 2: %v", err)
	}
	ms, err := st.ListUserMemberships(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListUserMemberships: %v", err)
	}
	if len(ms) != 2 || ms[0].TeamID != t1.ID || ms[0].Role != TeamRoleOwner || ms[1].TeamID != t2.ID {
		t.Fatalf("memberships = %+v", ms)
	}
	if empty, err := st.ListUserMemberships(ctx, "01NOBODY000000000000000000"); err != nil || len(empty) != 0 {
		t.Fatalf("empty memberships = %v err=%v", empty, err)
	}
}
