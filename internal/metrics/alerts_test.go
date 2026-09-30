package metrics

// vmalert 组件（metrics 栈第四件，W5-S2，D-V3W5-1）的收敛管理器单测：alerts.mode
// on/off 的收敛与清场、规则 config 内容寻址换版（增删换——scrape config 同
// 款）、vmalert spec 参数钉定（datasource/notifier/basicAuth/rule/求值周期
// ——flag 形态经镜像 -help 实测）、未装配接收器面的显式失败、规则渲染的
// 确定性。

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/componentloop"
	"github.com/fleetlyrun/fleetly/internal/state"
)

// newAlertsHarness 是告警面测试的公共装配（metrics harness + 接收器面注入
// ——生产装配的 notifier URL/tokenFile 形态在 provides.go）。
func newAlertsHarness(t *testing.T) *harness {
	t.Helper()
	h := newAlertsBaseHarness(t)
	h.mgr = h.mgr.WithAlertsNotifier("http://127.0.0.1:8420"+"/internal/alerts", filepath.Join(t.TempDir(), "fleetly-ingress.token"))
	return h
}

// seedRule 落一条规则（SaveAlertsSettings 前置门要求 metrics 先 on）。
func (h *harness) seedRule(name, expr string, forSeconds int64, labels map[string]string, channels []string) state.AlertRule {
	h.t.Helper()
	if err := h.st.SaveMetricsSettings(context.Background(), state.MetricsModeOn, state.MetricsSaveOptions{Actor: "test"}); err != nil {
		h.t.Fatalf("save metrics on: %v", err)
	}
	r, err := h.st.CreateAlertRule(context.Background(), state.AlertRuleWrite{
		Name: name, Expr: expr, ForDurationSeconds: forSeconds, Labels: labels, Channels: channels,
		Actor: "test",
	})
	if err != nil {
		h.t.Fatalf("create rule %s: %v", name, err)
	}
	return r
}

// TestVMAlertConvergesOnAlertsMode alerts.mode=on：三件之外加部署 vmalert
//（第 4 件）+ 规则 config；spec 参数逐项钉定；deployed 事件 ×4。
func TestVMAlertConvergesOnAlertsMode(t *testing.T) {
	h := newAlertsHarness(t)
	h.setMode(state.MetricsModeOn)
	if err := h.st.SaveAlertsSettings(context.Background(), state.AlertsModeOn, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save alerts on: %v", err)
	}
	h.seedRule("high-cpu", "cpu_used > 90", 300, map[string]string{"severity": "critical"}, []string{"ep-1"})

	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(h.fk.created) != 4 {
		t.Fatalf("created = %v, want 4 (three + vmalert)", h.fk.created)
	}
	if !h.fk.services[VMAlertServiceName].Exists {
		t.Fatal("vmalert not created")
	}
	cur := h.fk.services[VMAlertServiceName]
	wantArgs := []string{
		"-datasource.url=http://127.0.0.1:8428",
		"-remoteRead.url=http://127.0.0.1:8428",
		"-notifier.url=http://127.0.0.1:8420/internal/alerts",
		"-notifier.tlsInsecureSkipVerify=true",
		"-notifier.basicAuth.username=fleetly",
		"-notifier.basicAuth.passwordFile=/etc/fleetly/notifier-token",
		"-rule=/etc/vmalert/rules/*.yaml",
		"-evaluationInterval=30s",
		"-httpListenAddr=127.0.0.1:8880",
	}
	if !componentloop.SameStrings(cur.Args, wantArgs) {
		t.Fatalf("vmalert args:\n got %v\nwant %v", cur.Args, wantArgs)
	}
	// 规则 config 引用 + token 文件只读挂载（凭据材料不进 spec——挂载源是
	// 路径不是 token 本体）。
	if len(cur.ConfigNames) != 1 || !strings.HasPrefix(cur.ConfigNames[0], rulesConfigPrefix) {
		t.Fatalf("config names = %v, want one %s* entry", cur.ConfigNames, rulesConfigPrefix)
	}
	if len(cur.Mounts) != 1 || !strings.HasSuffix(cur.Mounts[0].Source, "fleetly-ingress.token") {
		t.Fatalf("mounts = %+v, want the token file bind", cur.Mounts)
	}
	// 规则 config 内容含渲染面事实（for/severity/channels annotation）。
	var rulesData string
	for _, spec := range h.fk.configs {
		if strings.HasPrefix(spec.Name, rulesConfigPrefix) {
			rulesData = string(spec.Data)
		}
	}
	for _, want := range []string{"alert: high-cpu", "for: 300s", "severity: critical", "channels: ep-1", "summary: high-cpu"} {
		if !strings.Contains(rulesData, want) {
			t.Fatalf("rules yaml missing %q:\n%s", want, rulesData)
		}
	}
	if len(h.eventsOf("metrics.stack_deployed")) != 4 {
		t.Fatalf("deployed events = %d, want 4", len(h.eventsOf("metrics.stack_deployed")))
	}

	// 第二拍：稳态零写（幂等）。
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if len(h.fk.created) != 4 || len(h.fk.updated) != 0 {
		t.Fatalf("steady state churn: created=%v updated=%v", h.fk.created, h.fk.updated)
	}
}

// TestRulesChangeSwapsConfig 规则变化 = 内容寻址 config 换版（新对象 +
// vmalert 服务更新 + 旧对象 GC）——scrape config 同款增删换。
func TestRulesChangeSwapsConfig(t *testing.T) {
	h := newAlertsHarness(t)
	h.setMode(state.MetricsModeOn)
	if err := h.st.SaveAlertsSettings(context.Background(), state.AlertsModeOn, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save alerts on: %v", err)
	}
	r := h.seedRule("rule-a", "up == 0", 0, nil, nil)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	oldName := h.fk.services[VMAlertServiceName].ConfigNames[0]

	// 规则 expr 更新 → 内容变 → config 名变 → 服务更新收敛。
	newExpr := "up == 1"
	if _, err := h.st.UpdateAlertRule(context.Background(), r.ID, state.AlertRuleUpdate{Expr: &newExpr}); err != nil {
		t.Fatalf("update rule: %v", err)
	}
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	newName := h.fk.services[VMAlertServiceName].ConfigNames[0]
	if newName == oldName {
		t.Fatal("rules config not swapped after rule change")
	}
	if _, ok := h.fk.configs[oldName]; ok {
		t.Fatalf("old rules config %s not gc-ed", oldName)
	}
	updates := 0
	for _, name := range h.fk.updated {
		if name == VMAlertServiceName {
			updates++
		}
	}
	if updates != 1 {
		t.Fatalf("vmalert updates = %d, want 1 (spec drift via config reference)", updates)
	}

	// 规则删除 → 换版到空规则集（内容寻址稳定，服务更新一次）。
	if err := h.st.DeleteAlertRule(context.Background(), r.ID, "test", ""); err != nil {
		t.Fatalf("delete rule: %v", err)
	}
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 3: %v", err)
	}
	if name := h.fk.services[VMAlertServiceName].ConfigNames[0]; name == newName {
		t.Fatal("rules config not swapped after rule delete")
	}
}

// TestVMAlertRemovedWhenAlertsOff alerts.mode 回 unset：vmalert 移除 + 规则
// config 清场，三件不动。
func TestVMAlertRemovedWhenAlertsOff(t *testing.T) {
	h := newAlertsHarness(t)
	h.setMode(state.MetricsModeOn)
	if err := h.st.SaveAlertsSettings(context.Background(), state.AlertsModeOn, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save alerts on: %v", err)
	}
	h.seedRule("rule-a", "up == 0", 0, nil, nil)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	if err := h.st.SaveAlertsSettings(context.Background(), state.AlertsModeUnset, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save alerts off: %v", err)
	}
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if h.fk.services[VMAlertServiceName].Exists {
		t.Fatal("vmalert still exists after alerts.mode off")
	}
	for _, name := range ComponentNames() {
		if !h.fk.services[name].Exists {
			t.Fatalf("service %s must stay deployed", name)
		}
	}
	for name := range h.fk.configs {
		if strings.HasPrefix(name, rulesConfigPrefix) {
			t.Fatalf("rules config %s not cleaned", name)
		}
	}
	// 清场差分事件（service 载荷——与 metrics off 的 volume_retained 形态区分）。
	found := false
	for _, ev := range h.eventsOf("metrics.stack_removed") {
		if strings.Contains(ev.Payload, `"service":"`+VMAlertServiceName+`"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("stack_removed events = %v, want one carrying the vmalert service", h.eventsOf("metrics.stack_removed"))
	}
}

// TestVMAlertRemovedWhenMetricsOff metrics.mode 回 unset：四件全清（vmalert
// 依赖 VM——removalOrder 把它排在最先删）。
func TestVMAlertRemovedWhenMetricsOff(t *testing.T) {
	h := newAlertsHarness(t)
	h.setMode(state.MetricsModeOn)
	if err := h.st.SaveAlertsSettings(context.Background(), state.AlertsModeOn, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save alerts on: %v", err)
	}
	h.seedRule("rule-a", "up == 0", 0, nil, nil)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	h.setMode(state.MetricsModeUnset)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if len(h.fk.removed) != 4 {
		t.Fatalf("removed = %v, want all four managed services", h.fk.removed)
	}
	if h.fk.removed[0] != VMAlertServiceName {
		t.Fatalf("first removed = %s, want vmalert (depends on the VM datasource)", h.fk.removed[0])
	}
}

// TestVMAlertRequiresNotifierAssembly alerts.mode=on 而接收器面未装配 →
// 显式失败退避（宁缺毋错——不部署一个 notifier 指向空的评估器）。
func TestVMAlertRequiresNotifierAssembly(t *testing.T) {
	h := newAlertsBaseHarness(t) // 不注入 WithAlertsNotifier
	h.setMode(state.MetricsModeOn)
	if err := h.st.SaveAlertsSettings(context.Background(), state.AlertsModeOn, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save alerts on: %v", err)
	}
	_, err := h.mgr.Ensure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not assembled") {
		t.Fatalf("Ensure err = %v, want explicit not-assembled failure", err)
	}
	if h.fk.services[VMAlertServiceName].Exists {
		t.Fatal("vmalert must not be deployed without the receiver face")
	}
}

// TestCheckHealthCoversVMAlert CheckHealth：alerts on 时 vmalert 缺席即红；
// alerts off 时无所欠。
func TestCheckHealthCoversVMAlert(t *testing.T) {
	h := newAlertsHarness(t)
	h.setMode(state.MetricsModeOn)
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// alerts off：vmalert 缺席不红。
	if err := h.mgr.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth (alerts off) = %v, want nil", err)
	}
	// alerts on：vmalert 缺席红（过渡态如实表达）。
	if err := h.st.SaveAlertsSettings(context.Background(), state.AlertsModeOn, state.AlertsSaveOptions{Actor: "test"}); err != nil {
		t.Fatalf("save alerts on: %v", err)
	}
	if err := h.mgr.CheckHealth(); err == nil {
		t.Fatal("CheckHealth must be red while alerts.mode=on and vmalert is absent")
	}
	// 收敛后转绿。
	if _, err := h.mgr.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure (alerts on): %v", err)
	}
	if err := h.mgr.CheckHealth(); err != nil {
		t.Fatalf("CheckHealth after converge = %v, want nil", err)
	}
}

// TestRulesYAMLDeterministic 渲染确定性：同规则集恒同内容（内容寻址的哈希
// 基），label 键与规则序（name 序）规范化。
func TestRulesYAMLDeterministic(t *testing.T) {
	rules := []state.AlertRule{
		{ID: "b", Name: "beta", Expr: "b > 1", Labels: map[string]string{"severity": "warning", "team": "x"}, Channels: []string{"ep2"}},
		{ID: "a", Name: "alpha", Expr: "a > 1\n", ForDurationSeconds: 60, Labels: map[string]string{"severity": "critical"}, Channels: []string{}},
	}
	y1 := rulesYAML(rules)
	y2 := rulesYAML([]state.AlertRule{rules[1], rules[0]}) // 输入序不影响渲染序
	if y1 != y2 {
		t.Fatalf("rules rendering is order-dependent:\n%s\n---\n%s", y1, y2)
	}
	alphaIdx := strings.Index(y1, "alert: alpha")
	betaIdx := strings.Index(y1, "alert: beta")
	if alphaIdx == -1 || betaIdx == -1 || alphaIdx > betaIdx {
		t.Fatalf("rules must render in name order:\n%s", y1)
	}
	for _, want := range []string{"for: 60s", "severity: critical", "team: x", "channels: ep2"} {
		if !strings.Contains(y1, want) {
			t.Fatalf("rules yaml missing %q:\n%s", want, y1)
		}
	}
	// 无 for（0 秒）不渲染 for 子句；空 channels 渲染空串 annotation。
	if strings.Contains(y1, "for: 0s") {
		t.Fatalf("zero for must not render:\n%s", y1)
	}
}

// newAlertsBaseHarness 是 metrics harness 的包内再导出（本文件聚合使用——
// 不带 notifier 注入的基形态，供未装配面测试）。
func newAlertsBaseHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InTx(context.Background(), func(tx *state.Tx) error {
		return tx.SetMeta(context.Background(), state.MetaKeyPlatformNodeID, "n_TESTNODEID01")
	}); err != nil {
		t.Fatalf("set meta: %v", err)
	}
	fk := newFakeDocker()
	fk.swarmActive = true
	fk.nodeAddrs = []string{"10.217.0.10"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := NewManagerWithDocker(st, 14, fk, logger)
	return &harness{t: t, st: st, fk: fk, mgr: mgr}
}
