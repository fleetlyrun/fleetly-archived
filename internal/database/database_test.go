package database

// Manager 收敛链的单测（假底座 + 真库）：覆盖验收清单的收敛器面——
// create→converge→ready（健康门通过）、健康门失败→failed+last_error、
// retry→ready、suspend→paused（副本 0 观察）→resume→ready、
// delete→reap→deleted（卷默认 orphaned / 显式删除 discarded）、
// degraded→recovered 观察、凭据 secret 的 PG 形态与明文零泄漏。
// CAS 冲突/错误码映射在 api 面单测（databases_test.go）。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/fleetlyrun/fleetly/internal/dbtemplate"
	"github.com/fleetlyrun/fleetly/internal/dutydocker"
	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/secrets"
	"github.com/fleetlyrun/fleetly/internal/state"
	testsupport "github.com/fleetlyrun/fleetly/internal/testsupport"
)

// fakeDocker 是 dockerPort 的假实现（收敛断言的记录器）。
type fakeDocker struct {
	active bool

	services map[string]dutydocker.ServiceSnapshot
	removed  []string
	updated  []string

	networks map[string]bool
	netUsed  map[string]int // 网络挂接端点计数（in-use 模拟）

	secrets map[string][]byte
	rmAcc   []string

	volumes map[string]bool

	tasks map[string][]dutydocker.TaskObservation

	// failRemove 让服务移除失败一次（幂等重试路径）。
	failRemove map[string]int

	// rotateRuns 记录一次性容器执行（轮换 job 断言面：cmd/env 携带新旧
	// 密码的形态——exitCode 是注入的作业退出码）。
	rotateRuns []ContainerRunInput
	rotateExit int

	// jobRuns 记录一次性 Swarm job 执行（S5 备份/恢复/校验/清理的断言面：
	// 镜像/挂载/网络/约束/env 的载荷形态）。jobMu 守卫 jobRuns：TriggerBackup
	// 的后台 goroutine（JobRun append）与测试轮询器（jobsWithPurpose 读）
	// 并发访问——race 检测面（2026-09-23 W1-S5 回归补钉，fixture 级）。
	jobMu   sync.Mutex
	jobRuns []JobRunInput
	// jobOutFn 按 job 载荷注入结论（nil = complete 空输出；测试按 Cmd 脚本
	// 内容分支——backup 注 restic --json summary，restore/verify 注失败）。
	jobOutFn func(in JobRunInput) JobRunOutcome
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		active:     true,
		services:   map[string]dutydocker.ServiceSnapshot{},
		networks:   map[string]bool{},
		netUsed:    map[string]int{},
		secrets:    map[string][]byte{},
		volumes:    map[string]bool{},
		tasks:      map[string][]dutydocker.TaskObservation{},
		failRemove: map[string]int{},
	}
}

func (f *fakeDocker) Info(context.Context) (dutydocker.InfoSnapshot, error) {
	return dutydocker.InfoSnapshot{SwarmActive: f.active}, nil
}

func (f *fakeDocker) ServiceInspect(_ context.Context, name string) (dutydocker.ServiceSnapshot, error) {
	s, ok := f.services[name]
	if !ok {
		return dutydocker.ServiceSnapshot{}, nil
	}
	return s, nil
}

func (f *fakeDocker) ServiceCreate(_ context.Context, spec swarm.ServiceSpec) error {
	f.services[spec.Name] = dutydocker.ServiceSnapshot{
		Exists:   true,
		Version:  1,
		Labels:   spec.Labels,
		Replicas: *spec.Mode.Replicated.Replicas,
	}
	return nil
}

func (f *fakeDocker) ServiceUpdate(_ context.Context, name string, _ uint64, spec swarm.ServiceSpec) error {
	f.updated = append(f.updated, name)
	f.services[name] = dutydocker.ServiceSnapshot{
		Exists:   true,
		Version:  2,
		Labels:   spec.Labels,
		Replicas: *spec.Mode.Replicated.Replicas,
	}
	return nil
}

func (f *fakeDocker) ServiceRemove(_ context.Context, name string) error {
	if n := f.failRemove[name]; n > 0 {
		f.failRemove[name] = n - 1
		return errors.New("simulated removal failure")
	}
	delete(f.services, name)
	f.removed = append(f.removed, name)
	return nil
}

func (f *fakeDocker) NetworkEnsure(_ context.Context, name string, _ bool) error {
	f.networks[name] = true
	return nil
}

func (f *fakeDocker) NetworkRemove(_ context.Context, name string) error {
	if f.netUsed[name] > 0 {
		return errors.New("network in use")
	}
	delete(f.networks, name)
	return nil
}

func (f *fakeDocker) SecretInspect(_ context.Context, name string) (string, bool, error) {
	if _, ok := f.secrets[name]; ok {
		return "sid-" + name, true, nil
	}
	return "", false, nil
}

func (f *fakeDocker) SecretCreate(_ context.Context, spec swarm.SecretSpec) (string, error) {
	f.secrets[spec.Name] = spec.Data
	return "sid-" + spec.Name, nil
}

func (f *fakeDocker) SecretList(_ context.Context, labels map[string]string) ([]string, error) {
	var out []string
	for name := range f.secrets {
		out = append(out, name)
	}
	return out, nil
}

func (f *fakeDocker) SecretRemove(_ context.Context, name string) error {
	delete(f.secrets, name)
	f.rmAcc = append(f.rmAcc, name)
	return nil
}

func (f *fakeDocker) VolumeEnsure(_ context.Context, name string) error {
	f.volumes[name] = true
	return nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, name string) error {
	delete(f.volumes, name)
	return nil
}

func (f *fakeDocker) TaskList(_ context.Context, service string) ([]dutydocker.TaskObservation, error) {
	return f.tasks[service], nil
}

func (f *fakeDocker) ContainerRun(_ context.Context, in ContainerRunInput) (int, error) {
	f.rotateRuns = append(f.rotateRuns, in)
	return f.rotateExit, nil
}

func (f *fakeDocker) JobRun(_ context.Context, in JobRunInput) (JobRunOutcome, error) {
	f.jobMu.Lock()
	f.jobRuns = append(f.jobRuns, in)
	f.jobMu.Unlock()
	if f.jobOutFn != nil {
		return f.jobOutFn(in), nil
	}
	return JobRunOutcome{State: "complete", ExitCode: 0}, nil
}

// jobsWithPurpose 按目的段筛 job 记录（fleetly-dbjob-<instance>-<purpose>-
// <ulid8> 命名——名字是载荷可识别性的契约面）。
func (f *fakeDocker) jobsWithPurpose(purpose string) []JobRunInput {
	f.jobMu.Lock()
	defer f.jobMu.Unlock()
	var out []JobRunInput
	for _, j := range f.jobRuns {
		if strings.Contains(j.Name, "-"+purpose+"-") {
			out = append(out, j)
		}
	}
	return out
}

// fakePlacement 是 PlacementSelector 的假实现（固定节点）。
type fakePlacement struct{ node string }

func (p fakePlacement) ResolveDatabase(_ context.Context, in DatabaseInput) (DatabaseDecision, error) {
	if in.ExistingBinding != "" {
		return DatabaseDecision{InstanceID: in.InstanceID, KeptExisting: true, PlatformNodeID: in.ExistingBinding}, nil
	}
	return DatabaseDecision{InstanceID: in.InstanceID, PlatformNodeID: p.node}, nil
}

// harness 是单测装配。
type harness struct {
	t      *testing.T
	st     *state.Store
	box    *secrets.Box
	docker *fakeDocker
	mgr    *Manager
	now    time.Time
	// proj 是夹具项目（v0.3 归属必填——实例行的 project_id/team_id 来源）；
	// teamSlug 是夹具团队 slug（三段命名公式的 team 段）。
	proj     state.Project
	teamSlug string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, _, err := secrets.EnsureKey(filepath.Join(dir, "test.key"))
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	fd := newFakeDocker()
	mgr := NewManagerWithDocker(Config{TickInterval: time.Hour, ProvisionTimeout: time.Hour},
		st, box, fakePlacement{node: "n_node"}, fd, slog.New(slog.DiscardHandler))
	mgr.upgradeWatch = 100 * time.Millisecond // 升级健康门观察窗（单测短窗）
	// now 钉定到本日 12:30 UTC 的确定性形态：缺省备份计划 hour_utc=3 的
	// 调度 duty 会在真实 03:xx UTC（本地 UTC+8 的 11 点档）萡入测试时给
	// ready 实例触发计划备份、占用操作互斥，后续收敛拍整体让位——2026-09-22
	// W5-S1 验收门复现的定时炸弹（其余时刻全绿故 W4 门未暴露）。需要窗口
	// 语义的测试显式设置 h.now（TestBackupSchedulingWindowAndPrune）。
	nowUTC := time.Now().UTC()
	proj := testsupport.SeedProject(t, st)
	team, err := st.GetTeam(context.Background(), proj.TeamID)
	if err != nil {
		t.Fatalf("get fixture team: %v", err)
	}
	h := &harness{t: t, st: st, box: box, docker: fd, mgr: mgr, proj: proj, teamSlug: team.Slug,
		now: time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 12, 30, 0, 0, time.UTC)}
	mgr.WithClock(func() time.Time { return h.now })
	return h
}

// waitUntil 轮询断言条件（异步编排 goroutine 的收口等待——超时 fail）。
func waitUntil(t *testing.T, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", budget, what)
}

// saveS3Settings 落库 external 模式的 S3 设置与 restic repo 口令（备份链
// 材料解析的最小前置——sk/repopass 经 envelope 加密落库）。
func (h *harness) saveS3Settings() {
	h.t.Helper()
	sk, err := h.box.Encrypt([]byte("test-s3-secret"))
	if err != nil {
		h.t.Fatalf("encrypt s3 secret: %v", err)
	}
	if err := h.st.SaveS3Settings(context.Background(), state.S3Settings{
		Mode:            state.S3ModeExternal,
		EndpointURL:     "http://s3.test:9000",
		Bucket:          "testbucket",
		AccessKeyID:     "testak",
		SecretAccessKey: string(sk),
		PathStyle:       true,
	}, state.S3SaveOptions{Actor: "human"}); err != nil {
		h.t.Fatalf("save s3 settings: %v", err)
	}
	pw, err := h.box.Encrypt([]byte("test-repo-pass"))
	if err != nil {
		h.t.Fatalf("encrypt restic password: %v", err)
	}
	if err := h.st.SaveResticPasswordCiphertext(context.Background(), string(pw)); err != nil {
		h.t.Fatalf("save restic password: %v", err)
	}
}

// resticSummaryOutput 是备份 job 的 restic --json 输出假体（summary 行 +
// 前置状态行——parseResticSummary 的解析面）。
func resticSummaryOutput(snap string, bytes int64) string {
	return "{\"message_type\":\"status\",\"current_files\":1}\n" +
		"{\"message_type\":\"summary\",\"snapshot_id\":\"" + snap + "\",\"total_bytes\":" + fmt.Sprint(bytes) + "}\n"
}

// createInstance 走与 API create 同构造的实例行（生成凭据 + 加密落库）。
func (h *harness) createInstance(name, template string) state.DatabaseInstance {
	h.t.Helper()
	password, err := dbtemplate.GeneratePassword()
	if err != nil {
		h.t.Fatalf("generate password: %v", err)
	}
	cipher, err := h.box.Encrypt([]byte(password))
	if err != nil {
		h.t.Fatalf("encrypt: %v", err)
	}
	tpl, err := dbtemplate.Get(template)
	if err != nil {
		h.t.Fatalf("template: %v", err)
	}
	inst, err := h.st.CreateDatabaseInstance(context.Background(), state.DatabaseInstance{
		Name:             name,
		Template:         template,
		ImageDigest:      tpl.Image,
		CredentialCipher: string(cipher),
		ProjectID:        h.proj.ID,
		TeamID:           h.proj.TeamID,
	})
	if err != nil {
		h.t.Fatalf("create instance: %v", err)
	}
	return inst
}

// get 重读实例行。
func (h *harness) get(id string) state.DatabaseInstance {
	h.t.Helper()
	inst, err := h.st.GetDatabaseInstance(context.Background(), id)
	if err != nil {
		h.t.Fatalf("get instance: %v", err)
	}
	return inst
}

// dbNetName 推导实例共享网络名（三段公式——夹具项目 slug + 实例名）。
func (h *harness) dbNetName(instance string) string {
	h.t.Helper()
	name, err := naming.DBNetworkName(h.teamSlug, h.proj.Slug, instance)
	if err != nil {
		h.t.Fatalf("db network name: %v", err)
	}
	return name
}

// svcName 解析实例的 swarm 服务名。
func (h *harness) svcName(inst state.DatabaseInstance) string {
	h.t.Helper()
	tpl, err := dbtemplate.Get(inst.Template)
	if err != nil {
		h.t.Fatalf("template: %v", err)
	}
	name, err := naming.DBServiceName(inst.TeamSlug, inst.ProjectSlug, inst.Name, tpl.ServiceName)
	if err != nil {
		h.t.Fatalf("service name: %v", err)
	}
	return name
}

// beatRun 跑一拍（带短超时预算的收敛拍）。
func (h *harness) beatRun() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.mgr.beat(ctx)
}

func (h *harness) setTasks(service string, tasks ...dutydocker.TaskObservation) {
	h.docker.tasks[service] = tasks
}

// TestProvisionToReady 覆盖 create→converge→ready：网络/卷/绑定/服务就位、
// 健康门通过 → ready、失败现场清空。
func TestProvisionToReady(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-main", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})

	h.beatRun()

	got := h.get(inst.ID)
	if got.State != state.DatabaseReady {
		t.Fatalf("state = %s, want ready", got.State)
	}
	if got.PlatformNodeID != "n_node" {
		t.Errorf("binding = %q, want n_node", got.PlatformNodeID)
	}
	if got.LastError != "" {
		t.Errorf("last_error = %q, want empty", got.LastError)
	}
	svc := h.docker.services[h.svcName(inst)]
	if !svc.Exists || svc.Replicas != 1 {
		t.Errorf("service = %+v, want exists replicas=1", svc)
	}
	if svc.Labels[state.LabelDesiredHash] == "" || svc.Labels[state.LabelDatabase] != inst.QualifiedName() {
		t.Errorf("service labels = %v, want desired-hash + fleetly.db marker (qualified form)", svc.Labels)
	}
	if !h.docker.networks[h.dbNetName("pg-main")] {
		t.Error("shared network not ensured")
	}
	// PG 凭据 secret：已创建、带归属 label 由 secretSpecOf 决定——名含指纹
	// hash8 且明文进创建载荷（这里只断言存在性；明文断言见 hash 一致性）。
	found := false
	for name := range h.docker.secrets {
		if strings.HasPrefix(name, "fleetly-db-"+h.teamSlug+"-"+h.proj.Slug+"-pg-main-password-") {
			found = true
		}
	}
	if !found {
		t.Error("PG credential secret not created")
	}
	// 卷登记（owner=database）。
	vols, err := h.st.ListOwnerVolumes(context.Background(), state.VolumeOwnerDatabase, inst.ID)
	if err != nil || len(vols) != 1 {
		t.Fatalf("volumes = %v err=%v, want 1 registered", vols, err)
	}
	if vols[0].PlatformNodeID != "n_node" || vols[0].Status != state.VolumeActive {
		t.Errorf("volume row = %+v, want pinned+active", vols[0])
	}
}

// TestProvisionHealthGateFailure 覆盖健康门失败 → failed + last_error（保
// 留现场：服务与卷不删）。
func TestProvisionHealthGateFailure(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-bad", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "rejected", DesiredState: "running",
		Err:   "image postgres:16@sha256:... not found",
		Image: inst.ImageDigest,
	})

	h.beatRun()

	got := h.get(inst.ID)
	if got.State != state.DatabaseFailed {
		t.Fatalf("state = %s, want failed", got.State)
	}
	if !strings.Contains(got.LastError, "image postgres:16") {
		t.Errorf("last_error = %q, want task error verbatim", got.LastError)
	}
	if !h.docker.services[h.svcName(inst)].Exists {
		t.Error("scene preserved: service must not be removed on provision failure")
	}
	// retry → provisioning → ready：last_error 清空。
	if err := h.st.EnterDbPhase(context.Background(), inst.ID,
		state.DatabaseFailed, state.DatabaseProvisioning); err != nil {
		t.Fatalf("retry transition: %v", err)
	}
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()
	got = h.get(inst.ID)
	if got.State != state.DatabaseReady || got.LastError != "" {
		t.Fatalf("after retry: state=%s last_error=%q, want ready+empty", got.State, got.LastError)
	}
}

// TestProvisionTimeout 覆盖健康门超时 → failed（预算用尽、任务无终态证据）。
func TestProvisionTimeout(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-slow", dbtemplate.TemplatePostgres16)
	h.beatRun() // 第一拍：进入 provisioning 记账（无任务 → pending）
	h.now = h.now.Add(2 * time.Hour)
	h.beatRun()
	got := h.get(inst.ID)
	if got.State != state.DatabaseFailed {
		t.Fatalf("state = %s, want failed (timeout)", got.State)
	}
	if !strings.Contains(got.LastError, "health gate timeout") {
		t.Errorf("last_error = %q, want timeout reason", got.LastError)
	}
}

// TestSuspendResume 覆盖 suspend → paused（收敛副本 0）→ resume → ready。
func TestSuspendResume(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("redis-main", dbtemplate.TemplateRedis7)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseReady {
		t.Fatalf("state = %s, want ready", got.State)
	}

	// suspend：API 已转 paused → 收敛落副本 0。
	if err := h.st.EnterDbPhase(context.Background(), inst.ID,
		state.DatabaseReady, state.DatabasePaused); err != nil {
		t.Fatalf("suspend transition: %v", err)
	}
	h.beatRun()
	svc := h.docker.services[h.svcName(inst)]
	if svc.Replicas != 0 {
		t.Fatalf("replicas = %d, want 0 (suspended)", svc.Replicas)
	}
	// 外部 scale 回 1 的漂移被纠回 0（下一拍）。
	svc = h.docker.services[h.svcName(inst)]
	svc.Replicas = 1
	h.docker.services[h.svcName(inst)] = svc
	h.beatRun()
	if got := h.docker.services[h.svcName(inst)]; got.Replicas != 0 {
		t.Fatalf("replicas after drift = %d, want 0", got.Replicas)
	}

	// resume：API 转回 provisioning → 收敛重建副本 1 → ready。
	if err := h.st.EnterDbPhase(context.Background(), inst.ID,
		state.DatabasePaused, state.DatabaseProvisioning); err != nil {
		t.Fatalf("resume transition: %v", err)
	}
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseReady {
		t.Fatalf("after resume: state = %s, want ready", got.State)
	}
	if got := h.docker.services[h.svcName(inst)]; got.Replicas != 1 {
		t.Fatalf("replicas after resume = %d, want 1", got.Replicas)
	}
}

// TestDegradedRecovered 覆盖在役不健康 → degraded、恢复 → ready。
func TestDegradedRecovered(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-watch", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()

	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "failed", DesiredState: "running",
		Err: "task: non-zero exit (1)", Image: inst.ImageDigest,
	})
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseDegraded {
		t.Fatalf("state = %s, want degraded", got.State)
	}

	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseReady {
		t.Fatalf("state = %s, want recovered to ready", got.State)
	}
}

// TestReapDefaultKeepsVolumes 覆盖 delete → reap → deleted：卷默认保留转
// orphaned、secret 清场、网络移除、终态事件。
func TestReapDefaultKeepsVolumes(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-del", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()
	vol := "fleetly-db-pg-del-data-" + inst.ID[:8]
	h.docker.volumes[vol] = true

	if err := h.st.EnterDbPhase(context.Background(), inst.ID,
		state.DatabaseReady, state.DatabaseDeleting); err != nil {
		t.Fatalf("delete transition: %v", err)
	}
	h.beatRun()

	got := h.get(inst.ID)
	if got.State != state.DatabaseDeleted {
		t.Fatalf("state = %s, want deleted", got.State)
	}
	if got.DeletedAt.IsZero() {
		t.Error("deleted_at not anchored")
	}
	if !h.docker.volumes[vol] {
		t.Error("volume must be kept by default (orphaned, not removed)")
	}
	if len(h.docker.rmAcc) == 0 {
		t.Error("credential secrets not reaped")
	}
	if h.docker.networks["fleetly-db-pg-del-net"] {
		t.Error("shared network not removed")
	}
	vols, err := h.st.ListOwnerVolumes(context.Background(), state.VolumeOwnerDatabase, inst.ID)
	if err != nil || len(vols) == 0 || vols[0].Status != state.VolumeOrphaned {
		t.Fatalf("volume rows = %+v err=%v, want orphaned", vols, err)
	}
}

// TestReapDeleteVolumes 覆盖 delete_volumes=true：底座卷移除 + 台账
// discarded。
func TestReapDeleteVolumes(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-purge", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()
	vol := "fleetly-db-pg-purge-data-" + inst.ID[:8]

	// 受理删除（API 同事务形态：置位 + 转移）。
	err := h.st.InTx(context.Background(), func(tx *state.Tx) error {
		if err := tx.SetDatabaseDeleteVolumes(context.Background(), inst.ID, true); err != nil {
			return err
		}
		return tx.EnterDbPhase(context.Background(), inst.ID,
			state.DatabaseReady, state.DatabaseDeleting)
	})
	if err != nil {
		t.Fatalf("accept delete: %v", err)
	}
	h.beatRun()

	if h.docker.volumes[vol] {
		t.Error("swarm volume must be removed when delete_volumes=true")
	}
	vols, _ := h.st.ListOwnerVolumes(context.Background(), state.VolumeOwnerDatabase, inst.ID)
	if len(vols) == 0 || vols[0].Status != state.VolumeDiscarded {
		t.Fatalf("volume rows = %+v, want discarded", vols)
	}
	if got := h.get(inst.ID); got.State != state.DatabaseDeleted {
		t.Fatalf("state = %s, want deleted", got.State)
	}
}

// TestReapRetriesIdempotently 覆盖 reap 的幂等重试：网络 in-use 挡一拍、
// 下一拍完成（deleting 无失败出边）。
func TestReapRetriesIdempotently(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-stuck", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()
	h.docker.netUsed[h.dbNetName("pg-stuck")] = 1 // 引用方端点未释放

	if err := h.st.EnterDbPhase(context.Background(), inst.ID,
		state.DatabaseReady, state.DatabaseDeleting); err != nil {
		t.Fatalf("delete transition: %v", err)
	}
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseDeleting {
		t.Fatalf("state = %s, want deleting (network in use, retry next beat)", got.State)
	}
	h.docker.netUsed[h.dbNetName("pg-stuck")] = 0
	h.beatRun()
	if got := h.get(inst.ID); got.State != state.DatabaseDeleted {
		t.Fatalf("state = %s, want deleted after retry", got.State)
	}
}

// TestCredentialPlaintextNeverLeaks 明文纪律负面测试：凭据明文不出现在
// Manager 日志缓冲可触达的任何持久面——这里对事件流做断言（事件是持久面；
// 日志由 slog.DiscardHandler 吞掉，事件/审计/台账是可断言的持久面全集）。
func TestCredentialPlaintextNeverLeaks(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-secret", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()

	plain, err := h.box.Decrypt([]byte(inst.CredentialCipher))
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	events, err := h.st.EventsSince(context.Background(), 0, 200)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, ev := range events {
		if strings.Contains(ev.Payload, string(plain)) {
			t.Fatalf("credential plaintext leaked into event %s payload", ev.Name)
		}
	}
}

// TestDesiredHashStable 覆盖幂等收敛：同态两拍不产生服务写（哈希判据）。
func TestDesiredHashStable(t *testing.T) {
	h := newHarness(t)
	inst := h.createInstance("pg-idem", dbtemplate.TemplatePostgres16)
	h.setTasks(h.svcName(inst), dutydocker.TaskObservation{
		State: "running", DesiredState: "running", Image: inst.ImageDigest,
	})
	h.beatRun()
	h.beatRun()
	if len(h.docker.updated) != 0 {
		t.Fatalf("stable state must not trigger service updates, got %v", h.docker.updated)
	}

	// 限额变更 → 哈希变化 → 下一拍服务更新（设置面由收敛器承载）。
	if err := h.st.UpdateDatabaseSettings(context.Background(), inst.ID, state.DatabaseSettings{
		CPUSeconds: 2.0, MemoryBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("update settings: %v", err)
	}
	h.beatRun()
	if len(h.docker.updated) != 1 {
		t.Fatalf("settings change must trigger exactly one service update, got %d", len(h.docker.updated))
	}
}
