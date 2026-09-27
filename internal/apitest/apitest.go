// Package apitest 是 CLI/SDK 侧集成测试的进程内服务装配夹具：真实
// internal/api 服务实现 + 独立临时目录 sqlite + 生产同构拦截链（auth）的
// bufconn gRPC server。它存在的理由（T2.18）：cmd/fleetly 测试需要「真实
// api 面」做 golden 快照与退出码断言，但 fence 禁止 cmd/fleetly 触及
// internal/state 等装配依赖——装配知识集中在本包，测试侧只拿连接与
// token。
//
// 注意：本包是测试支持设施，不是产品代码；随服务面扩展同步登记注册。
package apitest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/oklog/ulid/v2"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/api"
	"github.com/fleetlyrun/fleetly/internal/compose"
	"github.com/fleetlyrun/fleetly/internal/engine"
	"github.com/fleetlyrun/fleetly/internal/logs"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/substrate"
)

// Env 是一次测试环境：连接、admin token 与底层 store（夹具播种用——仅
// 测试代码经它写行，被测 CLI 只走 gRPC 面）。
type Env struct {
	Conn       *grpc.ClientConn
	AdminToken string
	Store      *state.Store
	Dir        string
	lis        *bufconn.Listener
}

// DialOptions 返回指向进程内 server 的拨号选项（CLI 测试经 extraDialOptions
// 注入——被测面与生产同路径走 fleetly.NewClient）。
func (e *Env) DialOptions() []grpc.DialOption {
	lis := e.lis
	return []grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
}

// fakeLogPort 是日志管线的确定性假底座端口：每个服务流首次轮询回放一行
// 固定日志（时间 = 注入时钟），随后永久阻塞——ring/落盘恰好一行，golden
// 可确定快照。
type fakeLogPort struct {
	line string
	at   time.Time
}

// fakeProjectNetworkPort 是项目网参与编排的确定性假端口（IMPL-T15-1）：
// ensure 恒成功；重部署入队返回 ErrNoRedeploySource（本环境无部署史——
// RPC 应答落 attached/detached 形态，CLI 断言面稳定）。
type fakeProjectNetworkPort struct{}

func (fakeProjectNetworkPort) EnsureProjectNetwork(context.Context, string) error { return nil }

func (fakeProjectNetworkPort) EnqueueNetworkRedeploy(context.Context, string) (string, error) {
	return "", engine.ErrNoRedeploySource
}

func (f *fakeLogPort) StreamServiceLogs(_ context.Context, _ string, _ time.Time, _ bool) (<-chan substrate.LogLine, error) {
	ch := make(chan substrate.LogLine)
	go func() {
		ch <- substrate.LogLine{At: f.at, Line: f.line}
		// 不 close、不再发送：模拟长驻日志流的拉取语义（range 阻塞）。
		block := make(chan struct{})
		<-block
	}()
	return ch, nil
}

func (f *fakeLogPort) ManagedServiceProcesses(_ context.Context, _ string) ([]string, error) {
	return []string{"web"}, nil
}

// JobServiceStates 实现 logs.Port 增补面（E5 Cron / DT-4）：API 测试装配无
// job 场景，恒空集。
func (f *fakeLogPort) JobServiceStates(_ context.Context, _ string) ([]engine.ServiceState, error) {
	return nil, nil
}

// Start 起一个完整服务面（除 ingress.Manager——nil 端口形态，入口面如实
// 报告不可用）并返回连接与 admin token；生命周期挂 t.Cleanup。
func Start(t *testing.T) *Env {
	return start(t, "", nil)
}

// StartWithJoin 起完整服务面并为 SystemService 注入 join 向导面（E1-8
// CLI 测试形态：base_domain 非空 + fake join token 端口——CLI 端到端走
// guide/rotate 路径，不触真实底座）。fake 端口行为：manager addr 固定
// 198.51.100.10；rotate 返回确定性新 token。
func StartWithJoin(t *testing.T) *Env {
	return start(t, "example.test", fakeJoinPort{})
}

// fakeJoinPort 是 join 向导面的确定性测试替身（api.JoinTokenPort 结构
// 同形实现——端口在 api 定义，apitest 侧最小实现）。
type fakeJoinPort struct{}

func (fakeJoinPort) SwarmJoinInfo(context.Context) (string, string, error) {
	return "198.51.100.10", "swmtkn-apitest-worker-token", nil
}

func (fakeJoinPort) SwarmRotateJoinToken(_ context.Context, role string) (string, error) {
	return "swmtkn-rotated-" + role, nil
}

// fakeGitHostKey 是 git SSH host key 指纹源的确定性测试替身（FZ-12 披露
// 面——api.GitHostKeySource 结构同形实现；指纹词形真实，golden 面的
// SHA256: 词头经 normalizeVolatile 归一）。
type fakeGitHostKey struct{}

func (fakeGitHostKey) Fingerprint() string { return "SHA256:FixturedGitHostKeyFingerprint==" }

// start 是 Start/StartWithJoin 的共用装配核。
func start(t *testing.T, joinBaseDomain string, joinPort api.JoinTokenPort) *Env {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("apitest: state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("apitest: EnsureKey: %v", err)
	}
	token := seedToken(t, st)

	// 日志管线：快扫描 + 确定性单行（fakeLogPort）。
	mgr := logs.NewManager(
		logs.Config{Dir: filepath.Join(dir, "logs"), ScanIntervalMillis: 20}.Normalize(),
		st, &fakeLogPort{line: "hello from web", at: time.Now().UTC().Truncate(time.Second).Add(time.Second)},
		box, slog.New(slog.NewTextHandler(os.Stderr, nil)),
	)
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Run(runCtx) }()

	// 引擎：nil 底座——drift 面的 show（无成功部署）/converge（无目标）/
	// enable/disable 路径不触底座；触底座路径属引擎集成测试职责。
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	eng := engine.NewEngine(engine.Config{}, st, nil, nil, nil, box, logger)

	auth := api.NewAuthenticator(st)
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(auth.UnaryAuthInterceptor()),
		grpc.ChainStreamInterceptor(auth.StreamAuthInterceptor()),
	)
	systemSvc := api.NewSystemService("dev", st,
		func() []api.SystemComponent { return nil }, nil, nil).WithGitHostKey(fakeGitHostKey{}).
		WithSecretsBox(box)
	if joinPort != nil {
		systemSvc = systemSvc.WithJoinGuide(joinBaseDomain, joinPort)
	}
	serverv1.RegisterSystemServiceServer(srv, systemSvc)
	serverv1.RegisterAppsServiceServer(srv, api.NewAppsService(st, box, "127.0.0.1:8424", nil))
	serverv1.RegisterDeploymentsServiceServer(srv, api.NewDeploymentsService(st, nil))
	serverv1.RegisterRevisionsServiceServer(srv, api.NewRevisionsService(st))
	serverv1.RegisterBuildsServiceServer(srv, api.NewBuildsService(st, nil))
	serverv1.RegisterDriftServiceServer(srv, api.NewDriftService(st, eng))
	serverv1.RegisterDomainsServiceServer(srv, api.NewDomainsService(st, nil))
	serverv1.RegisterEnvServiceServer(srv, api.NewEnvService(st, box))
	serverv1.RegisterLogsServiceServer(srv, api.NewLogsService(st, mgr))
	serverv1.RegisterEventsServiceServer(srv, api.NewEventsService(st))
	// 放置面：nil resolver = 只读降级形态（apitest 只装配读面；写面 RPC
	// 如实报不可用——生产装配在 internal/runtime/provides.go）。
	serverv1.RegisterPlacementServiceServer(srv, api.NewPlacementService(st, nil))
	serverv1.RegisterTokensServiceServer(srv, api.NewTokensService(st))
	serverv1.RegisterGitKeysServiceServer(srv, api.NewGitKeysService(st))
	// 库实例面（E4 W4-S2）：CLI golden/冒烟测试同路径消费（受理面——收敛
	// 行为在 internal/database 单测，本环境不装配 duty；kicker/rotator/ops
	// nil = 收敛由周期拍兜底、rotate/备份/恢复/升级 RPC 显式报错的降级形
	// 态，与生产 nil-safety 同语义）。密钥库面（W4-S4）：同路径消费（无值
	// 读回——负面扫描的 CLI 断言面）。
	serverv1.RegisterDatabaseServiceServer(srv, api.NewDatabaseService(st, box, nil, nil, nil))
	serverv1.RegisterSecretsServiceServer(srv, api.NewSecretsService(st, box))
	// 明文配置资源面（T 线 OT-3/IMPL-T1-4）：CLI/集成测试同路径消费
	//（受理/投影面；Get 明文回读的 admin 门在 scope 登记，与生产同形）。
	serverv1.RegisterConfigsServiceServer(srv, api.NewConfigsService(st))
	// 通知 Webhook 面（E6 W5-S4；W4-S3 通道扩展）：CLI golden/冒烟测试同
	// 路径消费（受理/投影面——投递器 duty 不在进程内装配，TestWebhook 指
	// 向真实网络才可达）。
	serverv1.RegisterNotificationsServiceServer(srv, api.NewNotificationsService(st, box))
	// 告警面（B 线 W5-S2，D-V3W5-1）：CLI 测试同路径消费（受理/投影面——
	// mb/mm nil = TestAlertRule 如实报不可用、status 部署态如实报 absent）。
	serverv1.RegisterAlertingServiceServer(srv, api.NewAlertingService(st))
	// metrics 面（E6 W5-S3）：alerts.mode 前置门的 CLI 驱动面（mb/mm nil =
	// status 部署态/节点比如实报 unset/0 的降级形态，与生产 nil-safety 同语义）。
	serverv1.RegisterMetricsServiceServer(srv, api.NewMetricsService(st))
	// 认证/用户面（v0.3 W1）：CLI/golden 测试同路径消费（注册/登录/会话；
	// 平台用户管理面在平台管理员判定后的读面）。
	serverv1.RegisterAuthServiceServer(srv, api.NewAuthService(st))
	serverv1.RegisterUsersServiceServer(srv, api.NewUsersService(st))
	// 审计读面（v0.3 W3-S1）：CLI 测试同路径消费（平台管理员双门在 handler，
	// 与生产同形）。
	serverv1.RegisterAuditServiceServer(srv, api.NewAuditService(st))
	// 团队/项目面（v0.3 W2-S1）：CLI/集成测试同路径消费（角色门在 handler
	// 内强制，与生产同形）。
	serverv1.RegisterTeamsServiceServer(srv, api.NewTeamsService(st))
	// 团队/项目面（v0.3 W2-S1）：CLI/集成测试同路径消费（角色门在 handler
	// 内强制，与生产同形）。IMPL-T15-1：项目网参与端口装配确定性假实现
	//（attach/detach 的 RPC 链可走通；网络/重部署编排语义在 engine/state
	// 各自测试覆盖——本环境无底座）。
	serverv1.RegisterProjectsServiceServer(srv, api.NewProjectsService(st).WithNetworkPort(fakeProjectNetworkPort{}))

	lis := bufconn.Listen(1024 * 1024)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("apitest: grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Env{Conn: conn, AdminToken: token, Store: st, Dir: dir, lis: lis}
}

// seedToken 落一枚 admin token（CLI 鉴权面凭据），返回明文。
func seedToken(t *testing.T, st *state.Store) string {
	t.Helper()
	plaintext, err := api.GenerateBootstrapAdminToken(context.Background(), st, "apitest admin")
	if err != nil {
		t.Fatalf("apitest: seed token: %v", err)
	}
	return plaintext
}

// SeedUser 注册一个用户（state.RegisterUser 原语——首注册 = 平台管理员 +
// 个人队 owner + 默认项目 default；后续注册需 auth.registration=open），
// 返回注册落位投影（CLI 登录态测试的身份/团队夹具）。
func (e *Env) SeedUser(t *testing.T, email, password string) state.RegisterResult {
	t.Helper()
	rr, err := e.Store.RegisterUser(context.Background(), state.RegisterWrite{Email: email, Password: password})
	if err != nil {
		t.Fatalf("apitest: register user %s: %v", email, err)
	}
	return rr
}

// SeedUserToken 为用户签发一枚用户 PAT（state.CreateToken 带 UserID——
// TokensService 用户化迁移是 W2 API 票面，登录态夹具在 state 原语层取数），
// 返回明文（格式与 api 面同形：flt_ + 24 字节 hex；scope read 够 Me 投影）。
func (e *Env) SeedUserToken(t *testing.T, userID string) string {
	t.Helper()
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("apitest: generate user token: %v", err)
	}
	plaintext := "flt_" + hex.EncodeToString(raw)
	if _, err := e.Store.CreateToken(context.Background(), state.TokenWrite{
		Hash:   state.HashToken(plaintext),
		Name:   "apitest user pat",
		Scopes: "read",
		UserID: userID,
		Actor:  "human",
	}); err != nil {
		t.Fatalf("apitest: create user token: %v", err)
	}
	return plaintext
}

// SeedProject 播种一个独立团队+项目（v0.3 W2-S3 归属管道：CreateApp 夹具
// 通道——每次调用建独立队/项目，避免 UNIQUE(project_id,name) 串扰）。slug
// 取 ULID 小写化片段（单词制词表内）。
func (e *Env) SeedProject(t *testing.T) state.Project {
	t.Helper()
	// slug 源 = ULID 整段随机区（第 11~26 字符）——Make() 同毫秒内单调递
	// 增，只有尾部可靠演化；取全随机区保证同毫秒连发不撞 team slug UNIQUE。
	suffix := strings.ToLower(ulid.Make().String())[10:26]
	team, err := e.Store.CreateTeam(context.Background(), state.TeamWrite{
		Slug: "t" + suffix, Name: "apitest fixture team", CreatedBy: "apitest",
	})
	if err != nil {
		t.Fatalf("apitest: seed fixture team: %v", err)
	}
	proj, err := e.Store.CreateProject(context.Background(), state.ProjectWrite{
		TeamID: team.ID, Slug: "p" + suffix, Name: "apitest fixture project",
	})
	if err != nil {
		t.Fatalf("apitest: seed fixture project: %v", err)
	}
	return proj
}

// CreateApp 播种一个应用行（读面测试的既有应用夹具；幂等——已存在时
// 返回既有行；新建行走独立夹具项目归属）。
func (e *Env) CreateApp(t *testing.T, name string) state.App {
	t.Helper()
	if app, err := e.Store.GetAppByName(context.Background(), name); err == nil {
		return app
	}
	proj := e.SeedProject(t)
	app, err := e.Store.CreateApp(context.Background(), "", name, proj.ID, proj.TeamID)
	if err != nil {
		t.Fatalf("apitest: create app %s: %v", name, err)
	}
	return app
}

// SeedRevisionFromYAML 经 compose.Load 解析夹具并固化一条成功版本快照
// （ComposeNormalized = canonical JSON——与引擎 succeedDeployment 同形态），
// 返回 revision ID。
func (e *Env) SeedRevisionFromYAML(t *testing.T, appName, yamlText string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(path, []byte(yamlText), 0o600); err != nil {
		t.Fatalf("apitest: write compose fixture: %v", err)
	}
	spec, _, err := compose.Load(context.Background(), path)
	if err != nil {
		t.Fatalf("apitest: load compose fixture: %v", err)
	}
	app := e.CreateApp(t, appName)
	raw, err := spec.CanonicalJSON()
	if err != nil {
		t.Fatalf("apitest: canonical json: %v", err)
	}
	var rev state.Revision
	err = e.Store.InTx(context.Background(), func(tx *state.Tx) error {
		r, err := tx.CreateRevision(context.Background(), state.RevisionWrite{
			AppID: app.ID, ComposeNormalized: string(raw),
			DesiredHash: spec.SpecHash,
		})
		rev = r
		return err
	})
	if err != nil {
		t.Fatalf("apitest: create revision: %v", err)
	}
	return rev.ID
}

// SeedPlacement 播种已绑定放置 + 已登记命名卷（placement show 夹具）。
// 应用以固定 ID 播种——卷名 `fleetly-<app>-<key>-<appid8>` 含 app ID 前 8
// 位，固定 ID 使 golden 快照确定。
func (e *Env) SeedPlacement(t *testing.T, appName string) string {
	t.Helper()
	app, err := e.Store.GetAppByName(context.Background(), appName)
	if err != nil {
		proj := e.SeedProject(t)
		app, err = e.Store.CreateApp(context.Background(), "01HJKMNP", appName, proj.ID, proj.TeamID)
		if err != nil {
			t.Fatalf("apitest: create fixed-id app: %v", err)
		}
	}
	platformID := "n_" + "01TESTNODE"
	if _, err := e.Store.BindPlacement(context.Background(), state.PlacementWrite{
		AppID: app.ID, PlatformNodeID: platformID,
		Source: state.PlacementSourcePlatform, Pinned: true,
	}); err != nil {
		t.Fatalf("apitest: bind placement: %v", err)
	}
	volName, err := naming.VolumeName(appName, "data", app.ID)
	if err != nil {
		t.Fatalf("apitest: volume name: %v", err)
	}
	if _, _, err := e.Store.RegisterAppVolume(context.Background(), state.VolumeWrite{
		AppID: app.ID, Key: "data", Name: volName,
		PlatformNodeID: platformID, MountPath: "/var/lib/data",
	}); err != nil {
		t.Fatalf("apitest: register volume: %v", err)
	}
	return platformID
}

// SeedNode 播种一条节点观测缓存行（nodes list 夹具）。
func (e *Env) SeedNode(t *testing.T) {
	t.Helper()
	err := e.Store.SyncNodeObservations(context.Background(), []state.SubstrateNode{{
		SwarmNodeID: "swarm-test", Hostname: "srv-01", State: "ready", Availability: "active",
		IsManager: true, Labels: map[string]string{state.LabelNodeID: "n_test"},
	}}, time.Now())
	if err != nil {
		t.Fatalf("apitest: seed node: %v", err)
	}
}

// SeedDomain 播种一条域名资源行（domains list 夹具）。
func (e *Env) SeedDomain(t *testing.T, appName, service, domain, port string) {
	t.Helper()
	app := e.CreateApp(t, appName)
	if _, err := e.Store.CreateAppDomain(context.Background(), app.ID, state.DomainInput{
		Domain: domain, Service: service, Port: port, Protocol: "http", CertMode: "http01",
	}); err != nil {
		t.Fatalf("apitest: seed domain: %v", err)
	}
}

// SeedBuild 播种一条 queued 构建行（builds list 夹具）。
func (e *Env) SeedBuild(t *testing.T, appName, service string) string {
	t.Helper()
	app := e.CreateApp(t, appName)
	rec, err := e.Store.CreateBuild(context.Background(), state.BuildRecord{
		AppID: app.ID, Service: service, Driver: state.DriverRailpack, Request: "{}",
	})
	if err != nil {
		t.Fatalf("apitest: seed build: %v", err)
	}
	return rec.ID
}

// SeedBackup 播种一条状态备份台账行（backups list 夹具；ID 按类别区分、
// 时间取按 kind 长度偏移的固定纪元——台账按 created_at DESC 排序，同刻
// 并列会造成顺序抖动，确定偏移使列表顺序稳定）。
func (e *Env) SeedBackup(t *testing.T, kind, verify, errMsg string) state.StateBackup {
	t.Helper()
	id := ("01BKTEST" + kind + "0000000000000000000000")[:26]
	at := time.Unix(1700000000, 0).UTC().Add(time.Duration(len(kind)) * time.Hour)
	rec, err := e.Store.RecordStateBackup(context.Background(), state.BackupWrite{
		ID:     id,
		At:     at,
		Kind:   kind,
		Path:   "/var/lib/fleetly/backups/" + id + "/fleetly.db",
		SHA256: strings.Repeat("a", 64),
		Size:   2048,
		Verify: verify,
		Error:  errMsg,
	})
	if err != nil {
		t.Fatalf("apitest: seed backup: %v", err)
	}
	return rec
}

// AppendEvent 播种一条注册表内事件（events watch 夹具）。
func (e *Env) AppendEvent(t *testing.T, name, subject, payload string) int64 {
	t.Helper()
	var seq int64
	err := e.Store.InTx(context.Background(), func(tx *state.Tx) error {
		s, err := tx.AppendEvent(context.Background(), state.Event{Name: name, Subject: subject, Payload: payload})
		seq = s
		return err
	})
	if err != nil {
		t.Fatalf("apitest: append event: %v", err)
	}
	return seq
}
