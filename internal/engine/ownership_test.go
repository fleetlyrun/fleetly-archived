package engine

// 归属对账域测试（IMPL-ARCH-A）：漂移判定的「期望集之外的多余受管服务也
// 是漂移」腿（extras 腿）必须按归属 label 的生产形态（三段限定形
// team/prj/app，W2-S3 起 planner 经 naming.ServiceLabels 写入）圈定受管集
// ——裸名过滤在真实集群恒空（W2-S3 回归缺陷，本文件红→绿钉死），同时钉死
// 两条豁免边界：任务承载服务（无 fleetly.app）与一次性 job 服务（cron/init
// 前缀族）不得被报 Extra。夹具一律经 naming 写入器造数（生产 label 形态）。

import (
	"context"
	"testing"

	"github.com/fleetlyrun/fleetly/internal/naming"
	"github.com/fleetlyrun/fleetly/internal/state"
	"github.com/fleetlyrun/fleetly/internal/testsupport"
)

// seedDeployedDemo 走发布主链把 demo 应用部署到 succeeded（extras 腿的期望
// 态来源），返回应用行（team/prj slug 随行，夹具造数的命名公式参数源）。
func seedDeployedDemo(t *testing.T, h *harness) state.App {
	t.Helper()
	if final := h.runToTerminal(h.enqueue(h.writeCompose(composeV1))); final.Status != state.DeploySucceeded {
		t.Fatalf("deploy = %s (%s)", final.Status, final.ErrorCode)
	}
	app := h.demoApp()
	return app
}

// serviceLabelsFor 是测试侧的 naming 写入器（生产 planner 同源——三段限定
// 形 fleetly.app 值；deployment label 是簿记键，夹具给占位值即可）。
func serviceLabelsFor(t *testing.T, app state.App, service, deploymentID string) map[string]string {
	t.Helper()
	labels, err := naming.ServiceLabels(app.TeamSlug, app.ProjectSlug, app.Name, service, deploymentID)
	if err != nil {
		t.Fatalf("service labels: %v", err)
	}
	return labels
}

// seedExternalService 把一只外部形态服务直接种进假底座（绕过发布管线——
// extras 腿的判定对象是「期望集之外」的实况，只能走底座注入缝）。
func seedExternalService(t *testing.T, h *harness, spec ServiceSpec) {
	t.Helper()
	if err := h.sub.ServiceCreate(context.Background(), spec); err != nil {
		t.Fatalf("seed service %s: %v", spec.Name, err)
	}
}

// TestDriftExtrasBeyondDesiredSet extras 腿三变体（IMPL-ARCH-A 红→绿）：
// 受管限定形多余服务报 Extra；任务承载服务与一次性 job 服务不报。
func TestDriftExtrasBeyondDesiredSet(t *testing.T) {
	t.Run("extra managed service with qualified app label is reported", func(t *testing.T) {
		h := newHarness(t)
		app := seedDeployedDemo(t, h)

		// 外部创建（或历史残留）的受管服务：label 经生产写入器——三段限定形。
		name, nerr := naming.ServiceName(app.TeamSlug, app.ProjectSlug, app.Name, "ghost")
		if nerr != nil {
			t.Fatalf("service name: %v", nerr)
		}
		seedExternalService(t, h, ServiceSpec{
			Name:          name,
			Image:         "alpine:3",
			Replicas:      1,
			ServiceLabels: serviceLabelsFor(t, app, "ghost", "depghost0001"),
		})

		report, err := h.eng.DriftShow(context.Background(), "demo")
		if err != nil {
			t.Fatalf("drift show: %v", err)
		}
		if !report.Drifted {
			t.Fatalf("extras leg dead: bare-name ownership filter misses qualified-label services (report: %+v)", report)
		}
		for _, s := range report.Services {
			if s.Service == name {
				if !s.Extra || !s.Drifted {
					t.Fatalf("service %s reported as %+v, want Extra+Drifted", name, s)
				}
				if len(s.Diff) != 1 || s.Diff[0].Field != "service" || s.Diff[0].Expected != "absent" || s.Diff[0].Actual != "present" {
					t.Fatalf("extra diff = %+v, want service absent/present", s.Diff)
				}
				return
			}
		}
		t.Fatalf("extra service %s missing from report: %+v", name, report.Services)
	})

	t.Run("task bearing service without app label is not extra", func(t *testing.T) {
		h := newHarness(t)
		seedDeployedDemo(t, h)

		// 任务承载服务（DT-5 生产形态）：fleetly.managed + 任务 label 集，
		// 无 fleetly.app（任务无 app 归属——state.LabelTasks 契约）。
		const taskID = "01ARZ3NDEKTSV4RRFFQ69G5FAV" // 26 位 ULID 形态（命名校验要求）
		name, nerr := naming.TaskServiceName(taskID)
		if nerr != nil {
			t.Fatalf("task service name: %v", nerr)
		}
		seedExternalService(t, h, ServiceSpec{
			Name:     name,
			Image:    "alpine:3",
			Replicas: 1,
			ServiceLabels: map[string]string{
				state.LabelManaged:   state.ManagedLabelValue,
				state.LabelTasks:     "true",
				state.LabelTaskID:    taskID,
				state.LabelTaskOwner: "tok_test000000000000000000",
				state.LabelTaskTTL:   "600",
			},
		})

		report, err := h.eng.DriftShow(context.Background(), "demo")
		if err != nil {
			t.Fatalf("drift show: %v", err)
		}
		if report.Drifted {
			t.Fatalf("task service misreported as drift: %+v", report.Services)
		}
		for _, s := range report.Services {
			if s.Service == name {
				t.Fatalf("task service %s must not be reported (no app ownership): %+v", name, s)
			}
		}
	})

	t.Run("one shot cron and init job services are not extra", func(t *testing.T) {
		h := newHarness(t)
		app := seedDeployedDemo(t, h)

		// 一次性 job 服务（E5 Cron / DT-4 生产形态）：经 JobSpecFrom 写方
		//（managed + 归属 + 进程 + 一次性运行锚），服务名走两个前缀族公式。
		template := ServiceSpec{
			Name:          h.svc("web"),
			Image:         "alpine:3",
			ServiceLabels: serviceLabelsFor(t, app, "web", "depjob00001"),
		}
		cronName, cerr := naming.CronJobName(app.TeamSlug, app.ProjectSlug, app.Name, "web", "run0000001")
		if cerr != nil {
			t.Fatalf("cron job name: %v", cerr)
		}
		seedExternalService(t, h, JobSpecFrom(template, cronName, map[string]string{
			state.LabelCronRun: "run00000000000000000001",
		}))
		initName, ierr := naming.InitJobName(app.TeamSlug, app.ProjectSlug, app.Name, "web", "depinit0001")
		if ierr != nil {
			t.Fatalf("init job name: %v", ierr)
		}
		seedExternalService(t, h, JobSpecFrom(template, initName, map[string]string{
			state.LabelInitRun: "depinit0001",
		}))

		report, err := h.eng.DriftShow(context.Background(), "demo")
		if err != nil {
			t.Fatalf("drift show: %v", err)
		}
		if report.Drifted {
			t.Fatalf("one shot job services misreported as drift: %+v", report.Services)
		}
		for _, s := range report.Services {
			if s.Service == cronName || s.Service == initName {
				t.Fatalf("one shot job service %s must be exempt from extras: %+v", s.Service, s)
			}
		}
	})
}

// TestScopeManagedServices 归属过滤 + 一次性 job 豁免的原语级钉死：限定形
// label 的长驻服务进结果；cron/init job 服务豁免；任务承载服务（无
// fleetly.app）与裸名 label 的异 app 服务不入结果。
func TestScopeManagedServices(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	app := seedDeployedDemo(t, h)

	// 长驻服务（期望集内，限定形 label——发布管线生产形态）。
	longrunning, nerr := naming.ServiceName(app.TeamSlug, app.ProjectSlug, app.Name, "web")
	if nerr != nil {
		t.Fatalf("service name: %v", nerr)
	}
	// 异 app 的受管服务（他 app 归属，不得混入本 app 作用域）。
	strangerApp := seedExternalApp(t, ctx, h.store)
	stranger, nerr := naming.ServiceName(strangerApp.TeamSlug, strangerApp.ProjectSlug, strangerApp.Name, "web")
	if nerr != nil {
		t.Fatalf("service name: %v", nerr)
	}
	seedExternalService(t, h, ServiceSpec{
		Name:          stranger,
		Image:         "alpine:3",
		Replicas:      1,
		ServiceLabels: serviceLabelsFor(t, strangerApp, "web", "depother01"),
	})
	// 一次性 job 服务（同挂本 app 限定形 label——豁免判据是前缀族）。
	cronName, cerr := naming.CronJobName(app.TeamSlug, app.ProjectSlug, app.Name, "web", "run0000042")
	if cerr != nil {
		t.Fatalf("cron job name: %v", cerr)
	}
	seedExternalService(t, h, JobSpecFrom(ServiceSpec{
		Name:          longrunning,
		Image:         "alpine:3",
		ServiceLabels: serviceLabelsFor(t, app, "web", "depjob00002"),
	}, cronName, map[string]string{state.LabelCronRun: "run00000000000000000042"}))
	initName, ierr := naming.InitJobName(app.TeamSlug, app.ProjectSlug, app.Name, "web", "depinit0002")
	if ierr != nil {
		t.Fatalf("init job name: %v", ierr)
	}
	seedExternalService(t, h, JobSpecFrom(ServiceSpec{
		Name:          longrunning,
		Image:         "alpine:3",
		ServiceLabels: serviceLabelsFor(t, app, "web", "depjob00003"),
	}, initName, map[string]string{state.LabelInitRun: "depinit0002"}))

	scoped, err := h.eng.scopeManagedServices(ctx, app)
	if err != nil {
		t.Fatalf("scope managed services: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Name != longrunning {
		t.Fatalf("scoped services = %+v, want exactly [%s]", namesOf(scoped), longrunning)
	}
}

// seedExternalApp 播种独立应用行（异 app 归属过滤的对照夹具；testsupport
// 夹具项目可重复——不同项目同名 app 合法，与主夹具互不串扰）。
func seedExternalApp(t *testing.T, ctx context.Context, st *state.Store) state.App {
	t.Helper()
	app, err := st.GetAppByName(ctx, "stranger")
	if err == nil {
		return app
	}
	if err != state.ErrAppNotFound {
		t.Fatalf("resolve stranger app: %v", err)
	}
	seeded, serr := testsupport.SeedAppE(t, st, "stranger")
	if serr != nil {
		t.Fatalf("seed stranger app: %v", serr)
	}
	return seeded
}

// namesOf 是服务实况投影的名字列（断言输出用）。
func namesOf(states []ServiceState) []string {
	out := make([]string, 0, len(states))
	for _, s := range states {
		out = append(out, s.Name)
	}
	return out
}
