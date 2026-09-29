package api

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	sharedv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/shared/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/state"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 状态层哨兵 → api 信封的统一映射（api 面错误语义的唯一登记点——IMPL-ARCH-G：
// 此前散布在约 20 个 handler 文件的 errors.Is → 信封映射收进本表，每个调用点
// 不必再自行知道「这个 state 方法回哪个哨兵、配哪个信封助手」）。
//
// 稳定码取舍：资源未找到（app/deployment/env/token 等）在 errcode 注册表
// 无对应码，本阶段 internal/errcode 不在允许改动清单——统一走**信封退化
// 形态**（code 空 + grpc code 机械映射：NotFound→404、FailedPrecondition→
// 409），与鉴权 401/403 同口径；如需稳定码须走注册表加码流程（遗留记录）。
// 表内已带注册码的行（E_DB_NOT_FOUND、E_WEBHOOK_NOT_FOUND 等）按注册表
// 投影 grpc/HTTP——stable code 只经 registry 的信封降级裁决原样保持；
// apperr 产出的错误（E_COMPOSE_*、E_ROLLBACK_NO_TARGET 等）原样透传
//（*apperr.Error 实现 GRPCStatus()，detail 信封随之上线）。
//
// 调用纪律：handler 对 state 层哨兵一律经 mapStoreErr 收口；表外哨兵原样
// 透传（不吞）。真正的特例（权限门 403 族、需要额外载荷的富投影）保留在
// 调用面，理由见各站点盘点；E_APP_AMBIGUOUS 候选列投影收编为本文件
// ambiguousRefErr 唯一构造点（IMPL-ARCH-L：六站同码异文案统一，见文末）。

// notFound 构造退化信封 NotFound。
func notFound(message string) error {
	return statusEnvelope(codes.NotFound, message)
}

// conflict 构造业务冲突信封（409）。X-5：携带最小 ErrorResponse detail
// （{"conflict": message}——B1 脱敏判定只认「无 detail」形态，携带 detail
// 即声明 message 是服务端构造的业务文案而非底层错误透传，REST 面原文
// 保留；无 detail 的 FailedPrecondition 会被误伤成固定文案 "internal
// error"）。空码 + FailedPrecondition 经 EnvelopeFromGRPCStatus 机械映射
// 回 409；gRPC/CLI 面原文与 detail 同上。
func conflict(message string) error {
	st := status.New(codes.FailedPrecondition, message)
	withDetail, err := st.WithDetails(&sharedv1.ErrorResponse{
		Message: message,
		Context: map[string]string{"conflict": message},
	})
	if err != nil {
		// detail 附加失败（理论不可达：proto 类型已注册）退化为纯 status
		// ——REST 面走退化信封路径（B1 兜底仍在）。
		return st.Err()
	}
	return withDetail.Err()
}

// statusInvalidArgument 构造退化信封无效请求（400——客户端可修正的输入
// 错误，如服务过滤名不存在）。
func statusInvalidArgument(message string) error {
	return statusEnvelope(codes.InvalidArgument, message)
}

// ── 哨兵登记表（IMPL-ARCH-G）─────────────────────────────────────────────────

// storeErrKind 是登记行的信封形态（哨兵命中时的产出形态）。
type storeErrKind int

const (
	// storeErrNotFound 退化信封 404（notFound；无 detail）。
	storeErrNotFound storeErrKind = iota
	// storeErrConflict 业务冲突空码 detail 信封 409（conflict）。
	storeErrConflict
	// storeErrInvalidArgument 退化信封 400（statusInvalidArgument）。
	storeErrInvalidArgument
	// storeErrPlainStatus 纯退化 status（statusEnvelope；grpc code 在行上
	// ——鉴权 401 族；无 detail）。
	storeErrPlainStatus
	// storeErrRegistered 注册表稳定码信封（apperr；码与 HTTP/grpc 投影都在
	// errcode 注册表——stable code 只经 registry）。
	storeErrRegistered
)

// storeErrContext 是 registered 行附带的结构化 context 键值（值来源三选一：
// 字面值 / 调用方首个参数 / 原错误链文本）。
type storeErrContext struct {
	key     string // context 键
	value   string // 字面值（fromArg/fromErr 均为 false 时生效；空串合法）
	fromArg bool   // 值 = 调用方首个参数（资源引用形态）
	fromErr bool   // 值 = err.Error()（原错误链文本）
}

// storeErrEntry 是一行登记：state 层哨兵 → api 信封产出。message 是该哨兵
// 的现行文案（逐字提取自迁移前各映射点；插值行带 %s/%q 动词，由调用方经
// args 供给值；固定文案行无动词）。code 仅 registered 行非空（退化信封不
// 发明码——stable code 只经 registry）。
type storeErrEntry struct {
	sentinel error
	kind     storeErrKind
	code     string            // registered 行的注册表码
	message  string            // 现行文案（format；调用方经 mapStoreErr 的 args 供值）
	grpcCode codes.Code        // plainStatus 行的 grpc code
	contexts []storeErrContext // registered 行附带 context
}

// storeErrTable 是哨兵 → 信封登记表。顺序无关（哨兵两两不互嵌——表测试
// 钉住每行恰好命中本行）；未登记的错误原样透传。
var storeErrTable = []storeErrEntry{
	// ── app 族（原 mapAppErr + apps.go）───────────────────────────────────
	{sentinel: state.ErrAppNotFound, kind: storeErrNotFound,
		message: "app not found: %s"},
	{sentinel: state.ErrAppTombstoned, kind: storeErrConflict,
		message: "app is tombstoned (deleting/deleted): %s"},
	{sentinel: state.ErrInvalidLifecycleTransition, kind: storeErrConflict,
		message: "app not deletable from current lifecycle: %s"},
	{sentinel: state.ErrAppSuspendedConflict, kind: storeErrRegistered, code: "E_APP_SUSPEND_CONFLICT",
		message:  "app suspend state changed concurrently: %s (re-read the app — the current projection is the truth)",
		contexts: []storeErrContext{{key: "conflict", fromErr: true}}},
	{sentinel: state.ErrAppExists, kind: storeErrConflict,
		message: "app %q already exists in the target project (names are unique per project); choose another target or rename first"},
	{sentinel: state.ErrScalingPolicyNotFound, kind: storeErrNotFound,
		message: "no scaling policy for service %q of app %q"},

	// ── build / config / secret / env / revision（app 附随资源）────────────
	{sentinel: state.ErrBuildNotFound, kind: storeErrNotFound,
		message: "build not found: %s"},
	{sentinel: state.ErrAppConfigNotFound, kind: storeErrNotFound,
		message: "config not found: %s"},
	{sentinel: state.ErrAppSecretNotFound, kind: storeErrNotFound,
		message: "secret not found: %s"},
	{sentinel: state.ErrEnvNotFound, kind: storeErrNotFound,
		message: "env var not found: %s"},
	{sentinel: state.ErrRevisionNotFound, kind: storeErrNotFound,
		message: "revision not found: %s"},

	// ── deployment / domain（发布与路由）──────────────────────────────────
	{sentinel: state.ErrDeploymentNotFound, kind: storeErrNotFound,
		message: "deployment not found: %s"},
	{sentinel: state.ErrDomainNotFound, kind: storeErrNotFound,
		message: "domain not found: %s"},
	{sentinel: state.ErrDomainConflict, kind: storeErrRegistered, code: "E_DOMAIN_CONFLICT",
		message:  "domain %q is already used by another service or app (a host belongs to exactly one service)",
		contexts: []storeErrContext{{key: "domain", fromArg: true}}},

	// ── 数据库族（D-DB-8 零新增码：NotFound 404 / CAS 冲突 409 / 终态不可见）──
	{sentinel: state.ErrDatabaseNotFound, kind: storeErrRegistered, code: "E_DB_NOT_FOUND",
		message:  "database instance not found",
		contexts: []storeErrContext{{key: "name"}, {key: "detail"}}},
	{sentinel: state.ErrDatabaseTerminal, kind: storeErrRegistered, code: "E_DB_NOT_FOUND",
		message:  "database instance not found (terminal state)",
		contexts: []storeErrContext{{key: "name"}, {key: "detail", value: "terminal state"}}},
	{sentinel: state.ErrDatabaseStateConflict, kind: storeErrRegistered, code: "E_STATE_VERSION_CONFLICT",
		message:  "database changed concurrently (CAS mismatch) — re-read the current state and retry with a legal prestate",
		contexts: []storeErrContext{{key: "conflict", fromErr: true}}},
	{sentinel: state.ErrDatabaseIllegalTransition, kind: storeErrRegistered, code: "E_STATE_VERSION_CONFLICT",
		message:  "database changed concurrently (CAS mismatch) — re-read the current state and retry with a legal prestate",
		contexts: []storeErrContext{{key: "conflict", fromErr: true}}},
	{sentinel: state.ErrDatabaseExists, kind: storeErrConflict,
		message: "database instance name %q is already registered (names stay reserved across the lifecycle)"},

	// ── 团队 / 项目 / 成员 / 邀请（rbac-teams §5 稳定码 + 退化信封）──────────
	{sentinel: state.ErrTeamNotFound, kind: storeErrNotFound,
		message: "team not found"},
	{sentinel: state.ErrTeamSlugTaken, kind: storeErrConflict,
		message: "team slug already taken"},
	{sentinel: state.ErrTeamNotEmpty, kind: storeErrConflict,
		message: "team still has projects; delete the (empty) projects first"},
	{sentinel: state.ErrTeamMemberExists, kind: storeErrConflict,
		message: "user is already a team member"},
	{sentinel: state.ErrTeamMemberNotFound, kind: storeErrNotFound,
		message: "team member not found"},
	{sentinel: state.ErrTeamLastOwner, kind: storeErrRegistered, code: "E_TEAM_LAST_OWNER",
		message: "cannot remove or demote the last owner of the team (grant the owner role to another member first)"},
	{sentinel: state.ErrInviteNotFound, kind: storeErrNotFound,
		message: "invite not found"},
	{sentinel: state.ErrInviteInvalid, kind: storeErrRegistered, code: "E_INVITE_INVALID",
		message: "invite is invalid, expired, or already used"},
	{sentinel: state.ErrNotTeamMember, kind: storeErrConflict,
		message: "target user is not a member of the owning team (project overrides are limited to team members)"},
	{sentinel: state.ErrProjectNotFound, kind: storeErrNotFound,
		message: "project not found"},
	{sentinel: state.ErrProjectSlugTaken, kind: storeErrConflict,
		message: "project slug already taken in team"},
	{sentinel: state.ErrProjectNotEmpty, kind: storeErrConflict,
		message: "project still has live resources (apps/databases); move or delete them first"},
	{sentinel: state.ErrProjectMemberNotFound, kind: storeErrNotFound,
		message: "project member override not found"},
	{sentinel: state.ErrProjectOwnerOverride, kind: storeErrConflict,
		message: "team owners cannot have a project role override (owners hold owner rights in every project)"},
	{sentinel: state.ErrDefaultProjectUnresolved, kind: storeErrInvalidArgument,
		message: `no default project resolvable for your account: pass project "team/project" explicitly`},

	// ── 任务（T 线；ErrTaskNotFound 三处同哨兵映射收敛为本行）───────────────
	{sentinel: state.ErrTaskNotFound, kind: storeErrNotFound,
		message: "task not found: %s"},

	// ── 认证 / 用户 / 令牌 / git key ─────────────────────────────────────
	{sentinel: state.ErrRegistrationClosed, kind: storeErrRegistered, code: "E_REGISTRATION_CLOSED",
		message:  "self-service registration is closed; ask a platform administrator to create the account",
		contexts: []storeErrContext{{key: "reason", value: "registration_closed"}}},
	{sentinel: state.ErrInvalidCredentials, kind: storeErrPlainStatus, grpcCode: codes.Unauthenticated,
		message: "invalid email or password"},
	{sentinel: state.ErrEmailTaken, kind: storeErrConflict,
		message: "email already registered: %s"},
	{sentinel: state.ErrUserNotFound, kind: storeErrNotFound,
		message: "user not found: %s"},
	{sentinel: state.ErrTokenNotFound, kind: storeErrNotFound,
		message: "token not found: %s"},
	{sentinel: state.ErrTokenLastAdmin, kind: storeErrRegistered, code: "E_TOKEN_LAST_ADMIN",
		message:  "token %s is the last non-revoked admin token; revoking it would leave the platform unmanageable (a restart does not re-seed the bootstrap token)",
		contexts: []storeErrContext{{key: "token", fromArg: true}, {key: "reason", value: "last_admin"}}},
	{sentinel: state.ErrGitKeyExists, kind: storeErrConflict,
		message: "git key already registered (same fingerprint)"},
	{sentinel: state.ErrGitKeyNotFound, kind: storeErrNotFound,
		message: "git key not found: %s"},

	// ── 告警 / webhook（通知面注册码投影）─────────────────────────────────
	{sentinel: state.ErrAlertRuleNotFound, kind: storeErrRegistered, code: "E_ALERT_RULE_NOT_FOUND",
		message: "alert rule not found"},
	{sentinel: state.ErrAlertRuleNameConflict, kind: storeErrRegistered, code: "E_ALERT_RULE_NAME_CONFLICT",
		message: "an alert rule with the same name already exists (rule names are unique platform-wide)"},
	{sentinel: state.ErrWebhookNotFound, kind: storeErrRegistered, code: "E_WEBHOOK_NOT_FOUND",
		message:  "webhook endpoint not found: %s",
		contexts: []storeErrContext{{key: "endpoint", fromArg: true}}},
	{sentinel: state.ErrWebhookNameConflict, kind: storeErrRegistered, code: "E_WEBHOOK_NAME_CONFLICT",
		message:  "a webhook endpoint named %q already exists (names are unique)",
		contexts: []storeErrContext{{key: "name", fromArg: true}}},
}

// mapStoreErr 是哨兵登记表的单点入口：命中表内哨兵即按登记行产出对应
// grpc/HTTP 信封（message = 行上现行文案 + 调用方 args）；未命中原样透传
// err（不吞、不包装——调用面特例先行判定后落到这里的错误保持底层形态）；
// err 为 nil 返回 nil。args 按行上 message 的动词供给插值值（固定文案行
// 不带 args）。
func mapStoreErr(err error, args ...any) error {
	if err == nil {
		return nil
	}
	for _, entry := range storeErrTable {
		if errors.Is(err, entry.sentinel) {
			return entry.render(err, args)
		}
	}
	return err
}

// render 按登记行产出信封错误（形态枚举见 storeErrKind；未知形态原样透传
// ——登记行形态漏项时不吞错误）。
func (e storeErrEntry) render(err error, args []any) error {
	message := fmt.Sprintf(e.message, args...)
	switch e.kind {
	case storeErrNotFound:
		return notFound(message)
	case storeErrConflict:
		return conflict(message)
	case storeErrInvalidArgument:
		return statusInvalidArgument(message)
	case storeErrPlainStatus:
		return statusEnvelope(e.grpcCode, message)
	case storeErrRegistered:
		projected := apperr.New(e.code, "%s", message)
		for _, c := range e.contexts {
			switch {
			case c.fromArg:
				if len(args) == 0 {
					// 登记行声明「值 = 调用方首参」而调用未供参：fail-fast
					//（与 apperr.New 对未注册码 panic 同纪律——构造期暴露
					// 调用面缺参，不带病产出空值信封）。
					panic(fmt.Sprintf("api: store error table row for %v expects the caller's first argument (context key %q)", e.sentinel, c.key))
				}
				projected = projected.WithContext(c.key, fmt.Sprint(args[0]))
			case c.fromErr:
				projected = projected.WithContext(c.key, err.Error())
			default:
				projected = projected.WithContext(c.key, c.value)
			}
		}
		return projected
	default:
		return err
	}
}

// ── E_APP_AMBIGUOUS 标准构造（IMPL-ARCH-L）──────────────────────────────────

// ambiguousRefErr 构造 E_APP_AMBIGUOUS 标准信封——全仓该注册码 detail 的
// 唯一构造点（基准措辞取自 ownership.resolveApp 的 appAmbiguousErr 先例，
// 用户裁决；此前的「reference it by id or use the qualified read face」
// 「…or the platform id」异形文案收编于此，此后改文案只改本处）。
// kindName 是资源类型词（app/database），兼任信封 context 键（六站既有键
// 逐一保持）；qualifiedForm 是可行动指引的限定形样例（team/prj/app、
// team/prj/db）；ref 是用户输入的引用值；candidates 是歧义行候选列（限定
// 形名，归属 slug 缺失的行退化列平台 ID），由站点枚举、本函数排序，非空
// 才投影 context（空列不投影——错误面不留空指引，非基准站点的信封形态
// 保持原样）。
func ambiguousRefErr(kindName, qualifiedForm, ref string, candidates []string) error {
	sort.Strings(candidates)
	projected := apperr.New("E_APP_AMBIGUOUS",
		"%s %q resolves to multiple rows across projects; use the %s qualified form",
		kindName, ref, qualifiedForm).
		WithContext(kindName, ref)
	if len(candidates) > 0 {
		projected = projected.WithContext("candidates", strings.Join(candidates, ","))
	}
	return projected
}
