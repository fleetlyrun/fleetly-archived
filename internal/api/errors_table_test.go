package api

// 哨兵登记表的表测试（IMPL-ARCH-G）。三件事：
//
//  1. 每行登记的产出钉死——grpc code / HTTP 状态 / registered code 有无 /
//     现行文案逐字比对（期望值硬编码自迁移前各映射点，与本包既有 handler
//     测试互为印证：表行漂移即红，映射写错被既有测试拦截）；
//  2. 哨兵两两不互嵌——每个哨兵恰好命中本行（无遮蔽，表序无关性成立）；
//  3. 表外行为钉死——未登记哨兵原样透传（同一错误值，不吞不包装），nil 进
//     nil 出；registered 行的码必在 errcode 注册表、退化行必无码（stable
//     code 只经 registry 的信封降级裁决）。

import (
	"errors"
	"fmt"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/errcode"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// storeErrExpectation 是一行人工期望（独立于 storeErrEntry 声明——两表不一致
// 即测试红，登记行自身字段不能自证）。
type storeErrExpectation struct {
	sentinel error
	args     []any  // 插值样例（与行上动词一一对应）
	message  string // 现行文案（迁移前逐字提取）
	grpc     codes.Code
	code     string // registered 码（空 = 退化信封，detail 无码）
	detailed bool   // 期望携带信封 detail（conflict/registered 两族）
}

// storeErrExpectations 与 errors.go 的 storeErrTable 逐行对应（顺序无关）。
// HTTP 状态断言：退化行经 GRPCCodeToHTTP 机械映射；registered 行按注册表
// （E_REGISTRATION_CLOSED 403 / *NOT_FOUND 404 / *CONFLICT·LAST_ADMIN·
// INVALID 409——errcode/codes.go 登记值硬编码于 registeredHTTP）。
var storeErrExpectations = []storeErrExpectation{
	{sentinel: state.ErrAppNotFound, args: []any{"demo"}, message: "app not found: demo", grpc: codes.NotFound},
	{sentinel: state.ErrAppTombstoned, args: []any{"demo"}, message: "app is tombstoned (deleting/deleted): demo", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrInvalidLifecycleTransition, args: []any{"demo"}, message: "app not deletable from current lifecycle: demo", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrAppSuspendedConflict, args: []any{"demo"}, message: "app suspend state changed concurrently: demo (re-read the app — the current projection is the truth)", grpc: codes.FailedPrecondition, code: "E_APP_SUSPEND_CONFLICT", detailed: true},
	{sentinel: state.ErrAppExists, args: []any{"demo"}, message: `app "demo" already exists in the target project (names are unique per project); choose another target or rename first`, grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrScalingPolicyNotFound, args: []any{"web", "demo"}, message: `no scaling policy for service "web" of app "demo"`, grpc: codes.NotFound},

	{sentinel: state.ErrBuildNotFound, args: []any{"b1"}, message: "build not found: b1", grpc: codes.NotFound},
	{sentinel: state.ErrAppConfigNotFound, args: []any{"cfg"}, message: "config not found: cfg", grpc: codes.NotFound},
	{sentinel: state.ErrAppSecretNotFound, args: []any{"s1"}, message: "secret not found: s1", grpc: codes.NotFound},
	{sentinel: state.ErrEnvNotFound, args: []any{"KEY"}, message: "env var not found: KEY", grpc: codes.NotFound},
	{sentinel: state.ErrRevisionNotFound, args: []any{"r1"}, message: "revision not found: r1", grpc: codes.NotFound},

	{sentinel: state.ErrDeploymentNotFound, args: []any{"d1"}, message: "deployment not found: d1", grpc: codes.NotFound},
	{sentinel: state.ErrDomainNotFound, args: []any{"a.example.com"}, message: "domain not found: a.example.com", grpc: codes.NotFound},
	{sentinel: state.ErrDomainConflict, args: []any{"a.example.com"}, message: `domain "a.example.com" is already used by another service or app (a host belongs to exactly one service)`, grpc: codes.FailedPrecondition, code: "E_DOMAIN_CONFLICT", detailed: true},

	{sentinel: state.ErrDatabaseNotFound, message: "database instance not found", grpc: codes.NotFound, code: "E_DB_NOT_FOUND", detailed: true},
	{sentinel: state.ErrDatabaseTerminal, message: "database instance not found (terminal state)", grpc: codes.NotFound, code: "E_DB_NOT_FOUND", detailed: true},
	{sentinel: state.ErrDatabaseStateConflict, message: "database changed concurrently (CAS mismatch) — re-read the current state and retry with a legal prestate", grpc: codes.FailedPrecondition, code: "E_STATE_VERSION_CONFLICT", detailed: true},
	{sentinel: state.ErrDatabaseIllegalTransition, message: "database changed concurrently (CAS mismatch) — re-read the current state and retry with a legal prestate", grpc: codes.FailedPrecondition, code: "E_STATE_VERSION_CONFLICT", detailed: true},
	{sentinel: state.ErrDatabaseExists, args: []any{"pg1"}, message: `database instance name "pg1" is already registered (names stay reserved across the lifecycle)`, grpc: codes.FailedPrecondition, detailed: true},

	{sentinel: state.ErrTeamNotFound, message: "team not found", grpc: codes.NotFound},
	{sentinel: state.ErrTeamSlugTaken, message: "team slug already taken", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrTeamNotEmpty, message: "team still has projects; delete the (empty) projects first", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrTeamMemberExists, message: "user is already a team member", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrTeamMemberNotFound, message: "team member not found", grpc: codes.NotFound},
	{sentinel: state.ErrTeamLastOwner, message: "cannot remove or demote the last owner of the team (grant the owner role to another member first)", grpc: codes.FailedPrecondition, code: "E_TEAM_LAST_OWNER", detailed: true},
	{sentinel: state.ErrInviteNotFound, message: "invite not found", grpc: codes.NotFound},
	{sentinel: state.ErrInviteInvalid, message: "invite is invalid, expired, or already used", grpc: codes.FailedPrecondition, code: "E_INVITE_INVALID", detailed: true},
	{sentinel: state.ErrNotTeamMember, message: "target user is not a member of the owning team (project overrides are limited to team members)", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrProjectNotFound, message: "project not found", grpc: codes.NotFound},
	{sentinel: state.ErrProjectSlugTaken, message: "project slug already taken in team", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrProjectNotEmpty, message: "project still has live resources (apps/databases); move or delete them first", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrProjectMemberNotFound, message: "project member override not found", grpc: codes.NotFound},
	{sentinel: state.ErrProjectOwnerOverride, message: "team owners cannot have a project role override (owners hold owner rights in every project)", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrDefaultProjectUnresolved, message: `no default project resolvable for your account: pass project "team/project" explicitly`, grpc: codes.InvalidArgument},

	{sentinel: state.ErrTaskNotFound, args: []any{"t1"}, message: "task not found: t1", grpc: codes.NotFound},

	{sentinel: state.ErrRegistrationClosed, message: "self-service registration is closed; ask a platform administrator to create the account", grpc: codes.PermissionDenied, code: "E_REGISTRATION_CLOSED", detailed: true},
	{sentinel: state.ErrInvalidCredentials, message: "invalid email or password", grpc: codes.Unauthenticated},
	{sentinel: state.ErrEmailTaken, args: []any{"a@b.c"}, message: "email already registered: a@b.c", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrUserNotFound, args: []any{"u1"}, message: "user not found: u1", grpc: codes.NotFound},
	{sentinel: state.ErrTokenNotFound, args: []any{"tok"}, message: "token not found: tok", grpc: codes.NotFound},
	{sentinel: state.ErrTokenLastAdmin, args: []any{"tok"}, message: "token tok is the last non-revoked admin token; revoking it would leave the platform unmanageable (a restart does not re-seed the bootstrap token)", grpc: codes.FailedPrecondition, code: "E_TOKEN_LAST_ADMIN", detailed: true},
	{sentinel: state.ErrGitKeyExists, message: "git key already registered (same fingerprint)", grpc: codes.FailedPrecondition, detailed: true},
	{sentinel: state.ErrGitKeyNotFound, args: []any{"k1"}, message: "git key not found: k1", grpc: codes.NotFound},

	{sentinel: state.ErrAlertRuleNotFound, message: "alert rule not found", grpc: codes.NotFound, code: "E_ALERT_RULE_NOT_FOUND", detailed: true},
	{sentinel: state.ErrAlertRuleNameConflict, message: "an alert rule with the same name already exists (rule names are unique platform-wide)", grpc: codes.FailedPrecondition, code: "E_ALERT_RULE_NAME_CONFLICT", detailed: true},
	{sentinel: state.ErrWebhookNotFound, args: []any{"wh1"}, message: "webhook endpoint not found: wh1", grpc: codes.NotFound, code: "E_WEBHOOK_NOT_FOUND", detailed: true},
	{sentinel: state.ErrWebhookNameConflict, args: []any{"wh1"}, message: `a webhook endpoint named "wh1" already exists (names are unique)`, grpc: codes.FailedPrecondition, code: "E_WEBHOOK_NAME_CONFLICT", detailed: true},
}

// registeredHTTP 是 registered 码 → 注册表 HTTP 状态（errcode/codes.go 登记
// 值硬编码——表行若改码/改码值即与此处失配）。
var registeredHTTP = map[string]int{
	"E_DOMAIN_CONFLICT":          409,
	"E_DB_NOT_FOUND":             404,
	"E_STATE_VERSION_CONFLICT":   409,
	"E_APP_SUSPEND_CONFLICT":     409,
	"E_TEAM_LAST_OWNER":          409,
	"E_INVITE_INVALID":           409,
	"E_REGISTRATION_CLOSED":      403,
	"E_TOKEN_LAST_ADMIN":         409,
	"E_WEBHOOK_NOT_FOUND":        404,
	"E_WEBHOOK_NAME_CONFLICT":    409,
	"E_ALERT_RULE_NOT_FOUND":     404,
	"E_ALERT_RULE_NAME_CONFLICT": 409,
}

// findRow 按哨兵取登记行（测试辅助：要求恰好一行命中）。
func findRow(t *testing.T, sentinel error) storeErrEntry {
	t.Helper()
	var hits []storeErrEntry
	for _, entry := range storeErrTable {
		if errors.Is(sentinel, entry.sentinel) {
			hits = append(hits, entry)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("sentinel %v matches %d table rows (want exactly 1)", sentinel, len(hits))
	}
	return hits[0]
}

// TestStoreErrTableCoversEveryRegisteredSentinel 钉住「期望表 ⇆ 登记表」双向
// 集相等：登记行不缺（漏登记的哨兵在调用面会被当表外透传）、不多（登记无
// 主的哨兵是死行）。
func TestStoreErrTableCoversEveryRegisteredSentinel(t *testing.T) {
	if len(storeErrExpectations) != len(storeErrTable) {
		t.Fatalf("expectation table has %d rows, storeErrTable has %d (the two must stay 1:1)", len(storeErrExpectations), len(storeErrTable))
	}
	seen := map[error]bool{}
	for _, exp := range storeErrExpectations {
		if seen[exp.sentinel] {
			t.Fatalf("sentinel %v listed twice in expectations", exp.sentinel)
		}
		seen[exp.sentinel] = true
		findRow(t, exp.sentinel) // 恰好命中一行
	}
	for _, entry := range storeErrTable {
		if !seen[entry.sentinel] {
			t.Fatalf("table row for %v has no expectation (add one — the registry and its pin must move together)", entry.sentinel)
		}
	}
}

// TestStoreErrTableNoShadowing 钉住哨兵两两不互嵌：任一登记哨兵对其他行哨兵
// errors.Is 不命中（表序无关性成立的前提；若 state 未来出现哨兵嵌套，此处
// 红即提示表需要显式排序语义）。
func TestStoreErrTableNoShadowing(t *testing.T) {
	for i, a := range storeErrTable {
		for j, b := range storeErrTable {
			if i != j && errors.Is(a.sentinel, b.sentinel) {
				t.Fatalf("sentinel %v also matches row for %v — table order would matter (shadowing)", a.sentinel, b.sentinel)
			}
		}
	}
}

// TestStoreErrTableProjections 逐行断言产出：grpc code、HTTP 状态、registered
// code 有无、detail 有无——「该哨兵错误 → 期望 grpc code/HTTP 状态 +
// registered code 有无」的票面验收句。
func TestStoreErrTableProjections(t *testing.T) {
	for _, exp := range storeErrExpectations {
		findRow(t, exp.sentinel)
		// 裹一层包装错误：映射必须沿 errors.Is 链命中（调用面拿到的是
		// state 原语返回的裹链错误）。
		got := mapStoreErr(fmt.Errorf("store: %w", exp.sentinel), exp.args...)
		if got == nil {
			t.Fatalf("%v: mapStoreErr returned nil", exp.sentinel)
		}
		if status.Code(got) != exp.grpc {
			t.Errorf("%v: grpc code = %v, want %v", exp.sentinel, status.Code(got), exp.grpc)
		}
		env, hasDetail := apperr.FromError(got)
		if hasDetail != exp.detailed {
			t.Errorf("%v: envelope detail present = %v, want %v", exp.sentinel, hasDetail, exp.detailed)
		}
		if hasDetail {
			if env.Code() != exp.code {
				t.Errorf("%v: envelope code = %q, want %q", exp.sentinel, env.Code(), exp.code)
			}
			if env.Message() != exp.message {
				t.Errorf("%v: message = %q, want %q (verbatim wording)", exp.sentinel, env.Message(), exp.message)
			}
		} else if msg := status.Convert(got).Message(); msg != exp.message {
			t.Errorf("%v: status message = %q, want %q (verbatim wording)", exp.sentinel, msg, exp.message)
		}
		// HTTP 状态：registered 行按注册表；退化行按 grpc code 机械映射。
		wantHTTP := 0
		if exp.code != "" {
			wantHTTP = registeredHTTP[exp.code]
			if wantHTTP == 0 {
				t.Fatalf("registered code %q missing from registeredHTTP pin", exp.code)
			}
		} else {
			wantHTTP = apperr.GRPCCodeToHTTP(exp.grpc)
		}
		gotHTTP := 0
		if ae := apperrFromError(got); ae != nil && ae.Code() != "" {
			gotHTTP = ae.HTTPStatus()
		} else {
			gotHTTP = apperr.GRPCCodeToHTTP(status.Code(got))
		}
		if gotHTTP != wantHTTP {
			t.Errorf("%v: HTTP status = %d, want %d", exp.sentinel, gotHTTP, wantHTTP)
		}
	}
}

// apperrFromError 是 apperr.FromError 的 *apperr.Error 投影（测试辅助）。
func apperrFromError(err error) *apperr.Error {
	var ae *apperr.Error
	errors.As(err, &ae)
	return ae
}

// TestStoreErrTableRegisteredContexts 断言 registered 行的结构化 context
// （值来源三形态：调用方首参 / 固定字面值 / 原错误链文本）。
func TestStoreErrTableRegisteredContexts(t *testing.T) {
	cases := []struct {
		sentinel error
		args     []any
		want     map[string]string
	}{
		{
			sentinel: state.ErrDatabaseNotFound,
			want:     map[string]string{"name": "", "detail": ""},
		},
		{
			sentinel: state.ErrDatabaseTerminal,
			want:     map[string]string{"name": "", "detail": "terminal state"},
		},
		{
			sentinel: state.ErrDatabaseStateConflict,
			want:     map[string]string{"conflict": "store: database state changed concurrently (CAS mismatch)"},
		},
		{
			sentinel: state.ErrWebhookNotFound,
			args:     []any{"wh1"},
			want:     map[string]string{"endpoint": "wh1"},
		},
		{
			sentinel: state.ErrWebhookNameConflict,
			args:     []any{"wh1"},
			want:     map[string]string{"name": "wh1"},
		},
		{
			sentinel: state.ErrDomainConflict,
			args:     []any{"a.example.com"},
			want:     map[string]string{"domain": "a.example.com"},
		},
		{
			sentinel: state.ErrTokenLastAdmin,
			args:     []any{"tok"},
			want:     map[string]string{"token": "tok", "reason": "last_admin"},
		},
		{
			sentinel: state.ErrRegistrationClosed,
			want:     map[string]string{"reason": "registration_closed"},
		},
	}
	for _, tc := range cases {
		got := mapStoreErr(fmt.Errorf("store: %w", tc.sentinel), tc.args...)
		ae := apperrFromError(got)
		if ae == nil {
			t.Fatalf("%v: not an apperr projection: %v", tc.sentinel, got)
		}
		ctx := ae.Context()
		for k, want := range tc.want {
			if ctx[k] != want {
				t.Errorf("%v: context[%q] = %q, want %q", tc.sentinel, k, ctx[k], want)
			}
		}
	}
}

// TestStoreErrTableStableCodeOnlyViaRegistry 钉住信封降级裁决：退化行产出
// 不携带任何码（stable code 只经 registry——登记表不得给退化行配码）；
// registered 行的码必在 errcode 注册表（apperr.New 构造期即校验，未注册
// panic——本测试通过即码全部在册）。
func TestStoreErrTableStableCodeOnlyViaRegistry(t *testing.T) {
	for _, entry := range storeErrTable {
		switch entry.kind {
		case storeErrRegistered:
			if entry.code == "" {
				t.Errorf("%v: registered row without a code", entry.sentinel)
			}
			if _, ok := errcode.Get(entry.code); !ok {
				t.Errorf("%v: code %q not in the errcode registry", entry.sentinel, entry.code)
			}
		default:
			if entry.code != "" {
				t.Errorf("%v: degraded row carries code %q (stable codes only come from the registry)", entry.sentinel, entry.code)
			}
		}
	}
}

// TestStoreErrUnregisteredPassthrough 钉住表外行为：未登记哨兵原样透传
// （同一错误值——不吞、不包装、不改语义），包装链不命中任何行，nil 进 nil
// 出。表外哨兵 = 调用面特例族的哨兵（权限门 403、E_APP_AMBIGUOUS 候选列、
// 幂等抑制、swarm 面、控制流跳过等——见各站点注记）。
func TestStoreErrUnregisteredPassthrough(t *testing.T) {
	unregistered := []error{
		state.ErrAppAmbiguous,      // 调用面 E_APP_AMBIGUOUS 候选列/异文案特例
		state.ErrDatabaseAmbiguous, // 同上（db 族）
		state.ErrMoveSameTarget,    // conflict(err.Error()) 裹链原文特例
		state.ErrTaskQuotaExceeded, // E_TASK_QUOTA_EXCEEDED 带 stage 载荷特例
		state.ErrSessionNotFound,   // Logout 幂等抑制特例
		state.ErrPlacementNotFound, // placement 缺省 = 视图不输出的控制流
		state.ErrNotSwarmManager,   // swarm 面异文案特例
	}
	for _, sentinel := range unregistered {
		if got := mapStoreErr(sentinel); got != sentinel {
			t.Errorf("unregistered sentinel %v: got %v, want the identical error value (passthrough)", sentinel, got)
		}
		wrapped := fmt.Errorf("store: %w", sentinel)
		if got := mapStoreErr(wrapped); got != wrapped {
			t.Errorf("unregistered wrapped %v: got %v, want the identical error value (passthrough)", sentinel, got)
		}
	}
	if got := mapStoreErr(nil); got != nil {
		t.Errorf("mapStoreErr(nil) = %v, want nil", got)
	}
}
