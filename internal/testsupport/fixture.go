// Package testsupport 是跨包测试夹具（v0.3 W2-S3 归属管道的测试通道）：
// 资产行归属（project_id/team_id）自 00019 起 NOT NULL——测试需要「先有
// 团队/项目，再有 app/db_instances 行」的两步播种；本包收敛该样板。只依赖
// state（不 import api/engine 等被测装配），测试包双向引用零循环。
//
// apitest 包同有 Env.SeedProject/Env.CreateApp（进程内服务装配夹具的成员
// 形态）——两者消费同一 state 原语，语义一致。
package testsupport

import (
	"context"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"github.com/fleetlyrun/fleetly/internal/state"
)

// SeedProject 播种一个独立团队+项目（每次调用建独立队/项目——避免
// UNIQUE(project_id,name) 与 UNIQUE(team_id,slug) 串扰；slug 取 ULID 小写
// 片段，单词制 [a-z0-9]{2,32} 词表内）。
func SeedProject(t *testing.T, st *state.Store) state.Project {
	t.Helper()
	return MustSeedProject(st)
}

// MustSeedProject 是 SeedProject 的无 t 形态（测试表驱动构造 helper）。
func MustSeedProject(st *state.Store) state.Project {
	// slug 源 = ULID 的随机区（第 11~22 字符，12 字符 ≈ 60 位随机）——Make()
	// 在同毫秒内是单调递增形态，只有尾部字符演化为可靠差异；12 字符保证同
	// 毫秒连发不撞 slug（team slug 全局 UNIQUE）。**不取到 26 位**：夹具团队
	// /项目 slug 是三段命名公式的 team/prj 段，17 字符（t/p + 16 随机）会把
	// 库服务名等拼过 swarm 64 上限（fleetly-db-<t>-<p>-pg-secret-postgres =
	// 65，2026-09-29 命名长度守卫落地时暴露——真 swarm 本就拒绝该名，fake
	// 引擎不校验长度把非法掩盖了）；夹具必须产出现实可命名的形态。
	suffix := strings.ToLower(ulid.Make().String())[10:22]
	team, err := st.CreateTeam(context.Background(), state.TeamWrite{
		Slug: "t" + suffix, Name: "fixture team", CreatedBy: "fixture",
	})
	if err != nil {
		panic("testsupport: seed fixture team: " + err.Error())
	}
	proj, err := st.CreateProject(context.Background(), state.ProjectWrite{
		TeamID: team.ID, Slug: "p" + suffix, Name: "fixture project",
	})
	if err != nil {
		panic("testsupport: seed fixture project: " + err.Error())
	}
	return proj
}

// SeedApp 在独立夹具项目下播种应用行（默认项目可重复——不同项目同名 app
// 合法，测试互不串扰）。
func SeedApp(t *testing.T, st *state.Store, name string) state.App {
	t.Helper()
	proj := SeedProject(t, st)
	app, err := st.CreateApp(context.Background(), "", name, proj.ID, proj.TeamID)
	if err != nil {
		t.Fatalf("testsupport: seed app %s: %v", name, err)
	}
	return app
}

// SeedAppE 是 SeedApp 的 (app, nil error) 签名形态：旧 CreateApp 三参调用
// 的机械迁移通道（err 恒 nil——既有 `if err != nil` 守卫保持合法且永不触
// 发；播种失败 = t.Fatalf 直接红）。
func SeedAppE(t *testing.T, st *state.Store, name string) (state.App, error) {
	t.Helper()
	return SeedApp(t, st, name), nil
}

// SeedAppInProject 在指定项目下播种应用行（跨项目同名/唯一性测试的精确
// 通道）。
func SeedAppInProject(t *testing.T, st *state.Store, name string, proj state.Project) state.App {
	t.Helper()
	app, err := st.CreateApp(context.Background(), "", name, proj.ID, proj.TeamID)
	if err != nil {
		t.Fatalf("testsupport: seed app %s in project: %v", name, err)
	}
	return app
}
