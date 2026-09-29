package api

// 多节点面测试（E1-6/E1-7/E1-8，multi-node §2.3/§2.6/§2.7）：
//   - GetJoinGuide：base_domain 缺失 → E_MULTI_NODE_REQUIRES_BASE_DOMAIN
//     （D-MN-13 门禁 409）；向导内容（join 命令/按 worker_ip 的规则/DNS/判据）。
//   - RotateJoinToken：轮换返回新 token + node.join_token_rotated 审计。
//   - ListNodes：NodeView.pinned_app_ids 读时 join placements 权威表。
//   - UpdatePlacement / ListVolumes / GetPlacementMigrationPlan：写面与
//     残留派生（裁决在 internal/placement，api 面做解析与投影）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	serverv1 "github.com/fleetlyrun/fleetly/genproto/fleetly/server/v1"
	"github.com/fleetlyrun/fleetly/internal/apperr"
	"github.com/fleetlyrun/fleetly/internal/placement"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// fakeJoin 是 JoinTokenPort 的测试替身。
type fakeJoin struct {
	addr  string
	token string
	calls int
	rErr  error
}

func (f *fakeJoin) SwarmJoinInfo(context.Context) (string, string, error) {
	return f.addr, f.token, nil
}

func (f *fakeJoin) SwarmRotateJoinToken(_ context.Context, role string) (string, error) {
	f.calls++
	if f.rErr != nil {
		return "", f.rErr
	}
	return "swmtkn-new-" + role, nil
}

// fakeDocker 是 placement.Resolver 需要的底座替身（直读节点快照）。
type fakeDocker struct {
	selfID string
	nodes  []state.SubstrateNode
}

func (f *fakeDocker) Ping(context.Context) error { return nil }
func (f *fakeDocker) ListNodeObservations(context.Context) ([]state.SubstrateNode, error) {
	return f.nodes, nil
}
func (f *fakeDocker) SelfNodeID(context.Context) (string, error) { return f.selfID, nil }
func (f *fakeDocker) UpdateNodeLabel(context.Context, string, string, string, state.ObjectVersion) error {
	return nil
}
func (f *fakeDocker) ResolveObjectVersion(context.Context, state.ObjectKind, string) (state.ObjectVersion, error) {
	return state.ObjectVersion{}, nil
}
func (f *fakeDocker) SubscribeEvents(context.Context) (<-chan state.SubstrateEvent, error) {
	return nil, errors.New("not used")
}
func (f *fakeDocker) Close() error { return nil }

// tStore 是 harness 的 store 取用别名（测试可读性）。
func tStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.Open(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestGetJoinGuideRequiresBaseDomain：base_domain 空 → 409 信封码
// E_MULTI_NODE_REQUIRES_BASE_DOMAIN（D-MN-13），且不触底座。
func TestGetJoinGuideRequiresBaseDomain(t *testing.T) {
	st := tStore(t)
	svc := NewSystemService("dev", st, nil, nil, nil).WithJoinGuide("", &fakeJoin{})
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srv, svc)
	conn := serveBufconn(t, srv)
	client := serverv1.NewSystemServiceClient(conn)

	tok := seedTokenPlain(t, st, "admin")
	_, err := client.GetJoinGuide(authCtx(context.Background(), tok),
		&serverv1.GetJoinGuideRequest{WorkerIp: "203.0.113.9"})
	if status.Code(err) != codes.Aborted && status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("grpc code = %v, want conflict-family (409 mapping)", status.Code(err))
	}
	ae, ok := apperr.FromGRPCStatus(status.Convert(err))
	if !ok || ae.Code() != "E_MULTI_NODE_REQUIRES_BASE_DOMAIN" {
		t.Fatalf("envelope = %v ok=%v, want E_MULTI_NODE_REQUIRES_BASE_DOMAIN", err, ok)
	}
}

// TestGetJoinGuideContent：向导材料断言——join 命令含 token 与 manager addr、
// 规则按 worker_ip 生成、ctrl.<base> 保持仅 manager、完成判据含 node.joined。
func TestGetJoinGuideContent(t *testing.T) {
	st := tStore(t)
	fj := &fakeJoin{addr: "198.51.100.10", token: "swmtkn-test-token"} //nolint:gosec // G101：测试夹具 token，非真实凭据
	svc := NewSystemService("dev", st, nil, nil, nil).WithJoinGuide("example.test", fj)
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srv, svc)
	conn := serveBufconn(t, srv)
	client := serverv1.NewSystemServiceClient(conn)

	tok := seedTokenPlain(t, st, "admin")
	resp, err := client.GetJoinGuide(authCtx(context.Background(), tok),
		&serverv1.GetJoinGuideRequest{WorkerIp: "203.0.113.9"})
	if err != nil {
		t.Fatalf("get join guide: %v", err)
	}
	g := resp.GetGuide()
	if !strings.Contains(g.GetJoinCommand(), "docker swarm join --token swmtkn-test-token") ||
		!strings.Contains(g.GetJoinCommand(), "198.51.100.10:2377") {
		t.Fatalf("join command = %q", g.GetJoinCommand())
	}
	if g.GetWorkerToken() != "swmtkn-test-token" || g.GetBaseDomain() != "example.test" {
		t.Fatalf("guide = %+v", g)
	}
	var managerRules, workerRules int
	for _, r := range g.GetManagerFirewallRules() {
		managerRules++
		if r.GetPort() == "2377/tcp" && !strings.Contains(r.GetRule(), "203.0.113.9") {
			t.Fatalf("2377 rule must embed worker ip: %q", r.GetRule())
		}
	}
	for range g.GetWorkerFirewallRules() {
		workerRules++
	}
	if managerRules < 5 || workerRules < 3 {
		t.Fatalf("rules = manager %d worker %d, want >=5 / >=3", managerRules, workerRules)
	}
	var ctrlOnly bool
	for _, s := range g.GetDnsSteps() {
		if strings.Contains(s, "ctrl.example.test") && strings.Contains(s, "manager only") {
			ctrlOnly = true
		}
	}
	if !ctrlOnly {
		t.Fatalf("dns steps must keep ctrl.<base> manager-only: %v", g.GetDnsSteps())
	}
	var joined bool
	for _, s := range g.GetCompletionChecks() {
		if strings.Contains(s, "node.joined") {
			joined = true
		}
	}
	if !joined {
		t.Fatalf("completion checks must mention node.joined: %v", g.GetCompletionChecks())
	}
	// manager_addr 覆盖生效。
	resp2, err := client.GetJoinGuide(authCtx(context.Background(), tok),
		&serverv1.GetJoinGuideRequest{WorkerIp: "203.0.113.9", ManagerAddr: "join.example.test:2377"})
	if err != nil {
		t.Fatalf("get join guide (override): %v", err)
	}
	if !strings.Contains(resp2.GetGuide().GetJoinCommand(), "join.example.test:2377") {
		t.Fatalf("manager addr override ignored: %q", resp2.GetGuide().GetJoinCommand())
	}
}

// TestRotateJoinTokenAudited：轮换经端口执行并落 node.join_token_rotated 审计。
func TestRotateJoinTokenAudited(t *testing.T) {
	st := tStore(t)
	fj := &fakeJoin{}
	svc := NewSystemService("dev", st, nil, nil, nil).WithJoinGuide("example.test", fj)
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srv, svc)
	conn := serveBufconn(t, srv)
	client := serverv1.NewSystemServiceClient(conn)

	tok := seedTokenPlain(t, st, "admin")
	resp, err := client.RotateJoinToken(authCtx(context.Background(), tok),
		&serverv1.RotateJoinTokenRequest{Role: "worker"})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if resp.GetToken() != "swmtkn-new-worker" || resp.GetRole() != "worker" {
		t.Fatalf("resp = %+v", resp)
	}
	// 缺省 role = worker。
	if _, err := client.RotateJoinToken(authCtx(context.Background(), tok),
		&serverv1.RotateJoinTokenRequest{}); err != nil {
		t.Fatalf("rotate default: %v", err)
	}
	rows, err := st.RecentAudits(context.Background(), 10)
	if err != nil {
		t.Fatalf("audits: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Action == "node.join_token_rotated" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("rotate audits = %d, want 2", n)
	}
}

// TestListNodesPinnedAppIds：NodeView.pinned_app_ids 读自 placements 权威
// 表（非空锚行全状态计入；未锚定节点为空清单）。
func TestListNodesPinnedAppIds(t *testing.T) {
	st := tStore(t)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, "web")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	if err := st.SyncNodeObservations(ctx, []state.SubstrateNode{
		{SwarmNodeID: "sw-1", Hostname: "srv-01", State: "ready", Availability: "active",
			Labels: map[string]string{state.LabelNodeID: "n_node1"}},
		{SwarmNodeID: "sw-2", Hostname: "srv-02", State: "ready", Availability: "active",
			Labels: map[string]string{}},
	}, time.Now().UTC()); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := st.BindPlacement(ctx, state.PlacementWrite{
		AppID: app.ID, PlatformNodeID: "n_node1", Source: state.PlacementSourcePlatform,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}

	svc := NewSystemService("dev", st, nil, nil, nil).WithJoinGuide("example.test", &fakeJoin{})
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterSystemServiceServer(srv, svc)
	conn := serveBufconn(t, srv)
	client := serverv1.NewSystemServiceClient(conn)
	tok := seedTokenPlain(t, st, "read")

	resp, err := client.ListNodes(authCtx(context.Background(), tok), &serverv1.ListNodesRequest{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	byHost := map[string][]string{}
	for _, n := range resp.GetNodes() {
		byHost[n.GetHostname()] = n.GetPinnedAppIds()
	}
	if len(byHost["srv-01"]) != 1 || byHost["srv-01"][0] != app.ID {
		t.Fatalf("pinned on srv-01 = %v, want [app]", byHost["srv-01"])
	}
	if len(byHost["srv-02"]) != 0 {
		t.Fatalf("unanchored node must have no pins: %v", byHost["srv-02"])
	}
}

// startPlacement 起一个带真实 resolver（fake 底座）的 PlacementService。
func startPlacement(t *testing.T, st *state.Store) (serverv1.PlacementServiceClient, *fakeDocker, string) {
	t.Helper()
	fd := &fakeDocker{
		selfID: "sw-1",
		nodes: []state.SubstrateNode{
			{SwarmNodeID: "sw-1", Hostname: "srv-01", State: "ready", Availability: "active",
				Labels: map[string]string{state.LabelNodeID: "n_node1"}},
			{SwarmNodeID: "sw-2", Hostname: "srv-02", State: "ready", Availability: "active",
				Labels: map[string]string{state.LabelNodeID: "n_node2"}},
		},
	}
	res := placement.NewResolver(st, fd)
	srv := newAuthServer(NewAuthenticator(st))
	serverv1.RegisterPlacementServiceServer(srv, NewPlacementService(st, res))
	conn := serveBufconn(t, srv)
	return serverv1.NewPlacementServiceClient(conn), fd, "sw-1"
}

// TestUpdatePlacementAndListVolumes：换点（restored）落绑定与卷 prev 登记；
// ListVolumes 派生 residual；过滤面生效。
func TestUpdatePlacementAndListVolumes(t *testing.T) {
	st := tStore(t)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, "web")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	client, _, _ := startPlacement(t, st)
	tok := seedTokenPlain(t, st, "admin")
	actx := authCtx(context.Background(), tok)

	// 初始绑定 + 卷登记在 n_node1。
	if _, err := st.BindPlacement(ctx, state.PlacementWrite{
		AppID: app.ID, PlatformNodeID: "n_node1", Source: state.PlacementSourcePlatform,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, _, err := st.RegisterAppVolume(ctx, state.VolumeWrite{
		AppID: app.ID, Key: "data", Name: "fleetly-web-data-aaaaaaaa", PlatformNodeID: "n_node1",
	}); err != nil {
		t.Fatalf("register volume: %v", err)
	}

	// 有卷缺 data_ack → E_VOLUME_NODE_MISMATCH（前哨语义前置到换点面）。
	_, missErr := client.UpdatePlacement(actx, &serverv1.UpdatePlacementRequest{App: "web", Node: "srv-02"})
	ae, ok := apperr.FromGRPCStatus(status.Convert(missErr))
	if !ok || ae.Code() != "E_VOLUME_NODE_MISMATCH" {
		t.Fatalf("err = %v, want E_VOLUME_NODE_MISMATCH envelope", missErr)
	}

	// restored 换点成功。
	up, err := client.UpdatePlacement(actx, &serverv1.UpdatePlacementRequest{
		App: "web", Node: "srv-02", DataAck: "restored",
	})
	if err != nil {
		t.Fatalf("update placement: %v", err)
	}
	if up.GetPlacement().GetPlatformNodeId() != "n_node2" {
		t.Fatalf("placement = %+v, want n_node2", up.GetPlacement())
	}
	if len(up.GetVolumes()) != 1 || up.GetVolumes()[0].GetPrevPlatformNodeId() != "n_node1" ||
		up.GetVolumes()[0].GetPlatformNodeId() != "n_node2" {
		t.Fatalf("volumes = %+v, want moved with prev", up.GetVolumes())
	}

	// ListVolumes：residual 派生 + status 过滤。
	lv, err := client.ListVolumes(actx, &serverv1.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if len(lv.GetVolumes()) != 1 || !lv.GetVolumes()[0].GetResidual() {
		t.Fatalf("volumes = %+v, want 1 residual row", lv.GetVolumes())
	}
	lvOrphan, err := client.ListVolumes(actx, &serverv1.ListVolumesRequest{Status: "orphaned"})
	if err != nil {
		t.Fatalf("list volumes (orphaned): %v", err)
	}
	if len(lvOrphan.GetVolumes()) != 0 {
		t.Fatalf("orphaned filter = %+v, want empty", lvOrphan.GetVolumes())
	}
}

// TestGetPlacementMigrationPlan：runbook 步骤含真实卷名与 rebind 收口；
// read scope 可读（非 admin）。
func TestGetPlacementMigrationPlan(t *testing.T) {
	st := tStore(t)
	ctx := context.Background()
	app, err := testsupport.SeedAppE(t, st, "web")
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	client, _, _ := startPlacement(t, st)
	tok := seedTokenPlain(t, st, "read")
	actx := authCtx(context.Background(), tok)

	if _, err := st.BindPlacement(ctx, state.PlacementWrite{
		AppID: app.ID, PlatformNodeID: "n_node1", Source: state.PlacementSourcePlatform,
	}); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if _, _, err := st.RegisterAppVolume(ctx, state.VolumeWrite{
		AppID: app.ID, Key: "data", Name: "fleetly-web-data-aaaaaaaa", PlatformNodeID: "n_node1",
	}); err != nil {
		t.Fatalf("register volume: %v", err)
	}

	resp, err := client.GetPlacementMigrationPlan(actx, &serverv1.GetPlacementMigrationPlanRequest{
		App: "web", To: "srv-02",
	})
	if err != nil {
		t.Fatalf("migration plan: %v", err)
	}
	var joined strings.Builder
	for _, s := range resp.GetSteps() {
		joined.WriteString(s.GetTitle() + "\n" + s.GetDetail() + "\n")
	}
	if !strings.Contains(joined.String(), "fleetly-web-data-aaaaaaaa") {
		t.Fatalf("plan must embed the real volume name: %s", joined.String())
	}
	if !strings.Contains(joined.String(), "placement rebind") {
		t.Fatalf("plan must end with the rebind step: %s", joined.String())
	}
	if resp.GetFromNode() == "" || !strings.Contains(resp.GetFromNode(), "n_node1") {
		t.Fatalf("from = %q, want current binding node", resp.GetFromNode())
	}
}
