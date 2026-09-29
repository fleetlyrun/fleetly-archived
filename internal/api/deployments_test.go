package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// ── 破坏性变更门控（MG-C3，架构 §2.4 plan/apply 语义）─────────────────────

// gateBaseCompose 基线：web + worker 两服务。
const gateBaseCompose = `
name: gateapp
services:
  web:
    image: nginx:1.27
  worker:
    image: my/worker
`

// gateRemovedCompose 删 worker（destructive）。
const gateRemovedCompose = `
name: gateapp
services:
  web:
    image: nginx:1.27
`

// gateChangedCompose 仅改 web 镜像（非 destructive）。
const gateChangedCompose = `
name: gateapp
services:
  web:
    image: nginx:1.28
  worker:
    image: my/worker
`

// seedGateRevision 落一条成功版本快照作为门控基线（ComposeNormalized =
// canonical JSON，与引擎 succeedDeployment 同形态），返回 app 行。app 行
// 播种在 env 夹具项目（projectRef 引用一致——Deploy 请求的归属校验）。
func seedGateRevision(t *testing.T, st *state.Store, yamlText string) state.App {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	spec, _, err := compose.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	proj := seedFixtureProject(t, st)
	app, err := st.CreateApp(context.Background(), "", spec.Name, proj.ID, proj.TeamID)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	raw, err := spec.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical json: %v", err)
	}
	if err := st.InTx(context.Background(), func(tx *state.Tx) error {
		_, err := tx.CreateRevision(context.Background(), state.RevisionWrite{
			AppID: app.ID, ComposeNormalized: string(raw), DesiredHash: spec.SpecHash,
		})
		return err
	}); err != nil {
		t.Fatalf("create revision: %v", err)
	}
	return app
}

// deploymentRowCount 数该 app 的部署行（「不入队」断言的读点）。
func deploymentRowCount(t *testing.T, st *state.Store, appName string) int {
	t.Helper()
	app, err := st.GetAppByName(context.Background(), appName)
	if err != nil {
		t.Fatalf("GetAppByName %s: %v", appName, err)
	}
	rows, err := st.ListAppDeployments(context.Background(), app.ID, 100)
	if err != nil {
		t.Fatalf("ListAppDeployments: %v", err)
	}
	return len(rows)
}

// TestDeployConfirmDestructiveGate MG-C3 机制测试：有历史 revision 且新
// compose 删除服务 → 无 confirm 被拒（E_DEPLOY_CONFIRM_REQUIRED 信封 +
// 修复建议文案，无部署行）；带 confirm → 入队成功；无服务删除（仅修改）
// → 不需要 confirm 即入队；首发（无 revision）恒放行。
func TestDeployConfirmDestructiveGate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	deploys := serverv1.NewDeploymentsServiceClient(env.conn)

	seedGateRevision(t, env.st, gateBaseCompose)

	// 删除服务、未确认：拒绝（信封 code、409 映射、建议文案含
	// --confirm-destructive），且不入队（部署行仍为 0）。app 行经夹具
	// 项目播种——请求 project 引用同一夹具项目（ID 形态，env.projectRef）。
	_, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(),
		App:     "gateapp",
		Compose: []byte(gateRemovedCompose),
	})
	if err == nil {
		t.Fatal("service-removing deploy without confirm must be rejected")
	}
	e, ok := apperr.FromError(err)
	if !ok {
		t.Fatalf("rejection error carries no envelope: %v", err)
	}
	if e.Code() != "E_DEPLOY_CONFIRM_REQUIRED" {
		t.Fatalf("envelope code = %q, want E_DEPLOY_CONFIRM_REQUIRED", e.Code())
	}
	if e.HTTPStatus() != 409 {
		t.Errorf("HTTP mapping = %d, want 409", e.HTTPStatus())
	}
	if sug := e.Envelope().GetSuggestion(); !strings.Contains(sug, "--confirm-destructive") {
		t.Errorf("suggestion text missing the --confirm-destructive retry hint: %q", sug)
	}
	if n := deploymentRowCount(t, env.st, "gateapp"); n != 0 {
		t.Fatalf("rejected deploy must not enqueue: deployment rows = %d", n)
	}

	// 同一 compose、带 confirm：入队成功（queued + 部署行 1）。
	dr, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(), 
		App:                "gateapp",
		Compose:            []byte(gateRemovedCompose),
		ConfirmDestructive: true,
	})
	if err != nil {
		t.Fatalf("confirmed deploy: %v", err)
	}
	if dr.GetStatus() != "queued" || dr.GetDeploymentId() == "" {
		t.Fatalf("confirmed receipt = %+v", dr)
	}
	if n := deploymentRowCount(t, env.st, "gateapp"); n != 1 {
		t.Fatalf("after confirm it should enqueue: deployment rows = %d, want 1", n)
	}

	// 仅修改（无服务删除）：不需要 confirm 即入队（部署行 2）。
	dr2, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(), 
		App:     "gateapp",
		Compose: []byte(gateChangedCompose),
	})
	if err != nil {
		t.Fatalf("modify-only deploy should not require confirm: %v", err)
	}
	if dr2.GetStatus() != "queued" {
		t.Fatalf("modify-only receipt = %+v", dr2)
	}
	if n := deploymentRowCount(t, env.st, "gateapp"); n != 2 {
		t.Fatalf("no service removal should enqueue: deployment rows = %d, want 2", n)
	}

	// 首发（无历史 revision）：无基线可比，恒放行。
	dr3, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(), 
		App:     "freshapp",
		Compose: []byte("name: freshapp\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err != nil {
		t.Fatalf("first deploy must pass the gate: %v", err)
	}
	if dr3.GetStatus() != "queued" {
		t.Fatalf("first deploy receipt = %+v", dr3)
	}
}

// TestDeployTempDirCleanedUp MG-6 回归：Deploy 的解析中转临时目录
// （os.MkdirTemp("fleetly-compose-")）随请求回收（defer os.RemoveAll）——
// 成功与被拒两条路径都不留孤儿 tmp（持久化副本在 <数据根>/deployments/
// <id>/compose.yaml，tmp 不是契约面）。
func TestDeployTempDirCleanedUp(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	deploys := serverv1.NewDeploymentsServiceClient(env.conn)

	countTmp := func() int {
		entries, err := os.ReadDir(os.TempDir())
		if err != nil {
			t.Fatalf("read temp dir: %v", err)
		}
		n := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "fleetly-compose-") {
				n++
			}
		}
		return n
	}

	before := countTmp()
	// 被拒路径（compose 名与请求 app 错位——在 ensureApp 之前返回）。
	if _, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(), 
		App:     "nomatch",
		Compose: []byte("name: otherapp\nservices:\n  web:\n    image: nginx:alpine\n"),
	}); err == nil {
		t.Fatal("app-name mismatch deploy must be rejected")
	}
	// 成功路径（入队）。
	if _, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(), 
		App:     "tmpclean",
		Compose: []byte("name: tmpclean\nservices:\n  web:\n    image: nginx:alpine\n"),
	}); err != nil {
		t.Fatalf("normal deploy: %v", err)
	}
	if after := countTmp(); after != before {
		t.Fatalf("parse-time staging temp dirs not reclaimed: fleetly-compose-* dir count %d → %d", before, after)
	}
}

// TestDeployAppNameMismatchRejected A1（S18）：compose 应用名与请求 app 不
// 一致 → E_COMPOSE_UNSUPPORTED（信封携带 expected/actual 上下文），且不
// 误建 app、不入队（拒绝发生在 ensureApp 之前）；一致 → 正常入队。
func TestDeployAppNameMismatchRejected(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	deploys := serverv1.NewDeploymentsServiceClient(env.conn)

	// 不一致：拒绝（REST 路径 {app}=urlapp、compose name=composeapp 的
	// 静默错位形态）。
	_, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(), 
		App:     "urlapp",
		Compose: []byte("name: composeapp\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err == nil {
		t.Fatal("app-name mismatch must be rejected")
	}
	e, ok := apperr.FromError(err)
	if !ok {
		t.Fatalf("rejection error carries no envelope: %v", err)
	}
	if e.Code() != "E_COMPOSE_UNSUPPORTED" {
		t.Fatalf("envelope code = %q, want E_COMPOSE_UNSUPPORTED", e.Code())
	}
	if e.Context()["expected"] != "urlapp" || e.Context()["actual"] != "composeapp" {
		t.Fatalf("envelope context = %v, want expected=urlapp actual=composeapp", e.Context())
	}

	// 不误建：两个名字的 app 行都不存在。
	for _, name := range []string{"urlapp", "composeapp"} {
		if _, gerr := env.st.GetAppByName(ctx, name); !errors.Is(gerr, state.ErrAppNotFound) {
			t.Fatalf("rejected request must not create app %s: %v", name, gerr)
		}
	}
	// 不入队：无任何在途部署行。
	if rows, lerr := env.st.ListNonTerminalDeployments(ctx); lerr != nil || len(rows) != 0 {
		t.Fatalf("rejected request must not enqueue: rows=%d err=%v", len(rows), lerr)
	}

	// 一致：正常入队（queued + 部署行 1）。
	dr, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(), 
		App:     "composeapp",
		Compose: []byte("name: composeapp\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err != nil {
		t.Fatalf("matching-names Deploy: %v", err)
	}
	if dr.GetStatus() != "queued" || dr.GetDeploymentId() == "" {
		t.Fatalf("matching receipt = %+v", dr)
	}
	if n := deploymentRowCount(t, env.st, "composeapp"); n != 1 {
		t.Fatalf("matching names should enqueue: deployment rows = %d, want 1", n)
	}
}

// TestDeployAppRefResolutionA1 A1 的引用解析语义（2026-09-25，Console 详情
// 导航自 5e593d9 起以平台 id 寻址 /v1/apps/{app}/deployments）：请求 app 能
// 解析到既有应用时与**解析后的业务名**比对（裸 ULID 与 compose name 必然错
// 位，原样比对使 Console 部署既有应用恒拒）；解析不到（=创建场景）保持原
// 语义——原始参数与 spec.Name 严格一致。四场景：
//  1. id 寻址 + compose 名一致 → 成功入队；
//  2. id 寻址 + compose 名不一致 → 拒（报错含业务名、不含平台 id）；
//  3. 创建场景裸名一致 → 成功入队；
//  4. 创建场景裸名不一致 → 拒（原始参数严格比对）。
func TestDeployAppRefResolutionA1(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	deploys := serverv1.NewDeploymentsServiceClient(env.conn)

	// 既有应用播种：经创建场景（裸名一致）部署一次落 app 行——后续场景以
	// 该行的平台 id 做 id 寻址。
	if _, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(),
		App:     "idaddr",
		Compose: []byte("name: idaddr\nservices:\n  web:\n    image: nginx:alpine\n"),
	}); err != nil {
		t.Fatalf("seed deploy: %v", err)
	}
	app, err := env.st.GetAppByName(ctx, "idaddr")
	if err != nil {
		t.Fatalf("GetAppByName idaddr: %v", err)
	}

	// 场景 1：id 寻址 + compose 名一致 → 成功入队（部署行累计 2）。
	dr, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(),
		App:     app.ID,
		Compose: []byte("name: idaddr\nservices:\n  web:\n    image: nginx:1.27\n"),
	})
	if err != nil {
		t.Fatalf("id-addressed matching deploy: %v", err)
	}
	if dr.GetStatus() != "queued" || dr.GetDeploymentId() == "" || dr.GetApp() != "idaddr" {
		t.Fatalf("id-addressed matching receipt = %+v", dr)
	}
	rowsBefore := deploymentRowCount(t, env.st, "idaddr")
	if rowsBefore != 2 {
		t.Fatalf("id-addressed matching deploy should enqueue: deployment rows = %d, want 2", rowsBefore)
	}

	// 场景 2：id 寻址 + compose 名不一致 → 拒（E_COMPOSE_UNSUPPORTED；报错
	// 展示解析后的业务名而非裸 ULID），且不入队、不误建 compose 侧名字。
	_, err = deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(),
		App:     app.ID,
		Compose: []byte("name: othername\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err == nil {
		t.Fatal("id-addressed mismatch deploy must be rejected")
	}
	e, ok := apperr.FromError(err)
	if !ok {
		t.Fatalf("rejection error carries no envelope: %v", err)
	}
	if e.Code() != "E_COMPOSE_UNSUPPORTED" {
		t.Fatalf("envelope code = %q, want E_COMPOSE_UNSUPPORTED", e.Code())
	}
	if msg := e.Envelope().GetMessage(); !strings.Contains(msg, "idaddr") || strings.Contains(msg, app.ID) {
		t.Fatalf("mismatch message must name the resolved app and not the platform id: %q", msg)
	}
	if e.Context()["expected"] != "idaddr" || e.Context()["actual"] != "othername" {
		t.Fatalf("envelope context = %v, want expected=idaddr actual=othername", e.Context())
	}
	if n := deploymentRowCount(t, env.st, "idaddr"); n != rowsBefore {
		t.Fatalf("rejected deploy must not enqueue: deployment rows = %d, want %d", n, rowsBefore)
	}
	if _, gerr := env.st.GetAppByName(ctx, "othername"); !errors.Is(gerr, state.ErrAppNotFound) {
		t.Fatalf("rejected request must not create app othername: %v", gerr)
	}

	// 场景 3：创建场景裸名一致 → 成功入队（resolveApp 解析不到 → 原始参数
	// 与 spec.Name 一致放行，ensureApp 建行）。
	dr2, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(),
		App:     "freshname",
		Compose: []byte("name: freshname\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err != nil {
		t.Fatalf("creation-scenario matching deploy: %v", err)
	}
	if dr2.GetStatus() != "queued" || dr2.GetDeploymentId() == "" {
		t.Fatalf("creation-scenario matching receipt = %+v", dr2)
	}
	if n := deploymentRowCount(t, env.st, "freshname"); n != 1 {
		t.Fatalf("creation-scenario matching deploy should enqueue: deployment rows = %d, want 1", n)
	}

	// 场景 4：创建场景裸名不一致 → 拒（原始参数严格比对语义保持——app 参数
	// 声明 X 不得静默建出 Y），且两个名字的 app 行都不存在。
	_, err = deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{Project: env.projectRef(),
		App:     "declaredname",
		Compose: []byte("name: builtname\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err == nil {
		t.Fatal("creation-scenario mismatch deploy must be rejected")
	}
	e, ok = apperr.FromError(err)
	if !ok {
		t.Fatalf("rejection error carries no envelope: %v", err)
	}
	if e.Code() != "E_COMPOSE_UNSUPPORTED" {
		t.Fatalf("envelope code = %q, want E_COMPOSE_UNSUPPORTED", e.Code())
	}
	if e.Context()["expected"] != "declaredname" || e.Context()["actual"] != "builtname" {
		t.Fatalf("envelope context = %v, want expected=declaredname actual=builtname", e.Context())
	}
	for _, name := range []string{"declaredname", "builtname"} {
		if _, gerr := env.st.GetAppByName(ctx, name); !errors.Is(gerr, state.ErrAppNotFound) {
			t.Fatalf("rejected request must not create app %s: %v", name, gerr)
		}
	}
}

// TestDeployBareNameAmbiguityCreationFallback A1 兜底语义的直接测试
//（2026-09-25 复核补测）：裸名解析域内歧义（E_APP_AMBIGUOUS）不是 Deploy
// 的拒绝面——Deploy 的兜底是「解析不到既有应用按创建语义：原始参数与
// spec.Name 一致才放行」。歧义面归属裁决在 ensureApp 的（目标项目，名）
// 精确查询：匹配部署命中目标项目的既有行，不误建第三行、不串扰其他项目
// 的同名行；错位部署照拒且零建行零入队。
func TestDeployBareNameAmbiguityCreationFallback(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	deploys := serverv1.NewDeploymentsServiceClient(env.conn)

	// 裸名歧义播种：同名 app 行落在两个项目（机具令牌全域解析 → 歧义）。
	projA := env.fixtureProject
	team, err := env.st.GetTeamBySlug(ctx, "tfixture")
	if err != nil {
		t.Fatalf("get fixture team: %v", err)
	}
	projB, err := env.st.CreateProject(ctx, state.ProjectWrite{
		TeamID: team.ID, Slug: "fixture2", Name: "fixture project 2",
	})
	if err != nil {
		t.Fatalf("create second project: %v", err)
	}
	for _, p := range []state.Project{projA, projB} {
		if _, cerr := env.st.CreateApp(ctx, "", "dupapp", p.ID, p.TeamID); cerr != nil {
			t.Fatalf("create dupapp in %s: %v", p.Slug, cerr)
		}
	}
	// 前置自证：裸名解析确实歧义（Deploy 的兜底分支以此为触发前提）。
	// directCtx 注入机具令牌等效 Principal（UserID 空 = 全域解析域）——
	// resolveApp 的 Principal 读自进程内 ctx 值，不跨界（harness_test.go
	// directCtx 注释同口径）。
	if _, rerr := resolveApp(directCtx(ctx), env.st, "dupapp"); rerr == nil {
		t.Fatal("precondition broken: bare name dupapp resolves without ambiguity")
	} else if e, ok := apperr.FromError(rerr); !ok || e.Code() != "E_APP_AMBIGUOUS" {
		t.Fatalf("precondition error = %v, want E_APP_AMBIGUOUS envelope", rerr)
	}

	countRowsByNameIn := func(p state.Project) int {
		t.Helper()
		app, gerr := env.st.GetAppByNameInProject(ctx, p.ID, "dupapp")
		if gerr != nil {
			t.Fatalf("GetAppByNameInProject %s/dupapp: %v", p.Slug, gerr)
		}
		rows, lerr := env.st.ListAppDeployments(ctx, app.ID, 100)
		if lerr != nil {
			t.Fatalf("ListAppDeployments: %v", lerr)
		}
		return len(rows)
	}

	// 放行面：原文与 spec.Name 一致 → 兜底放行；ensureApp 按请求 project
	// 精确命中 projB 的既有行——部署落在 projB，projA 同名行不受扰，无第三行。
	dr, err := deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{
		Project: projB.ID,
		App:     "dupapp",
		Compose: []byte("name: dupapp\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err != nil {
		t.Fatalf("ambiguous bare-name matching deploy must fall through to creation semantics: %v", err)
	}
	if dr.GetStatus() != "queued" || dr.GetDeploymentId() == "" || dr.GetApp() != "dupapp" {
		t.Fatalf("ambiguous matching receipt = %+v", dr)
	}
	rows, err := env.st.ListAppRowsByName(ctx, "dupapp")
	if err != nil || len(rows) != 2 {
		t.Fatalf("matching deploy must not create a third dupapp row: rows=%d err=%v", len(rows), err)
	}
	if n := countRowsByNameIn(projB); n != 1 {
		t.Fatalf("deployment must land on the requested project's row: projB rows = %d, want 1", n)
	}
	if n := countRowsByNameIn(projA); n != 0 {
		t.Fatalf("deployment must not touch the sibling row: projA rows = %d, want 0", n)
	}

	// 拒绝面：原文与 spec.Name 错位 → E_COMPOSE_UNSUPPORTED（expected=dupapp
	// 原始参数口径），且零建行、不入队（在途行维持放行面的 1 条）。
	_, err = deploys.Deploy(authCtx(ctx, env.depTok), &serverv1.DeployRequest{
		Project: projB.ID,
		App:     "dupapp",
		Compose: []byte("name: othername\nservices:\n  web:\n    image: nginx:alpine\n"),
	})
	if err == nil {
		t.Fatal("ambiguous bare-name mismatch deploy must be rejected")
	}
	e, ok := apperr.FromError(err)
	if !ok {
		t.Fatalf("rejection error carries no envelope: %v", err)
	}
	if e.Code() != "E_COMPOSE_UNSUPPORTED" {
		t.Fatalf("envelope code = %q, want E_COMPOSE_UNSUPPORTED", e.Code())
	}
	if e.Context()["expected"] != "dupapp" || e.Context()["actual"] != "othername" {
		t.Fatalf("envelope context = %v, want expected=dupapp actual=othername", e.Context())
	}
	rows, err = env.st.ListAppRowsByName(ctx, "othername")
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected request must not create app othername: rows=%d err=%v", len(rows), err)
	}
	if ntid, lerr := env.st.ListNonTerminalDeployments(ctx); lerr != nil || len(ntid) != 1 {
		t.Fatalf("rejected request must not enqueue: non-terminal rows=%d err=%v, want 1", len(ntid), lerr)
	}
}
