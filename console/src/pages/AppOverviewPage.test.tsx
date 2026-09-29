// 应用概览页测试（E3-8/E5 Cron 切面）：服务清单从最近 active revision 的
// 归一化快照现读——cron 服务如实标注 scheduled（不冒充长驻态），长驻服务
// 不做状态冒充；cron 区块（台账 + 手动触发）挂载于概览页。平台管理员双门
//（P0-3 残余面收口）：cron 触发 / metrics 开关 / 扩缩策略写钮隐藏、原位
// 只读说明；非管理员 owner 零变化（防回归）。部署入口区（§4.7）的专测在
// AppOverviewPage.deploy.test.tsx——本文件只钉装配守卫（owner 见 Deploy
// settings + Deploy 卡；平台管理员说明态 + 无写钮）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { AppOverviewPage } from "@/pages/AppOverviewPage";
import { TeamProjectProvider } from "@/lib/context";
import { setToken } from "@/api/client";

const COMPOSE = JSON.stringify({
  name: "demo",
  services: [
    { name: "web", image: "nginx:1.27" },
    { name: "jobber", cron: { expression: "*/5 * * * *" } },
  ],
});

const RUNS = [
  {
    id: "run_ok",
    service: "jobber",
    expression: "*/5 * * * *",
    scheduled_at: "2026-09-21T02:00:00Z",
    finished_at: "2026-09-21T02:00:10Z",
    status: "succeeded",
  },
];

function stubOverviewFetch(
  log: { url: string; method: string }[] = [],
  appExtra: Record<string, unknown> = {},
) {
  return vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    log.push({ url, method: init?.method ?? "GET" });
    if (url.endsWith("/spec")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ compose: COMPOSE }),
      });
    }
    if (init?.method === "DELETE" && url.endsWith("/apps/demo")) {
      // DeleteAppResponse：{name, lifecycle}（tombstone 第一拍已提交）。
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ name: "demo", lifecycle: "deleting" }),
      });
    }
    if (url.includes("/cron-runs")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ app: "demo", runs: RUNS }),
      });
    }
    if (url.includes("/services/jobber/trigger")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({
            run: { id: "run_manual", service: "jobber", status: "started" },
          }),
      });
    }
    if (url.includes("/apps/demo/revisions")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({ revisions: [{ id: "rev1", status: "active" }] }),
      });
    }
    if (url.includes("/placement")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ volumes: [] }),
      });
    }
    // GET /apps/demo（GetAppResponse）
    return Promise.resolve({
      ok: true,
      status: 200,
      statusText: "",
      json: () =>
        Promise.resolve({
          id: "a1",
          name: "demo",
          lifecycle: "active",
          derived_state: "running",
          ...appExtra,
        }),
    });
  });
}

function renderOverview(app = "demo") {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={[`/apps/${app}`]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route path="/apps/:name" element={<AppOverviewPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("AppOverviewPage services list (E5 Cron)", () => {
  it("marks cron services scheduled (not running) and lists long-running services plainly", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubOverviewFetch());

    renderOverview();

    await screen.findByTestId("services-list");
    const badges = await screen.findAllByTestId("service-scheduled-badge");
    expect(badges).toHaveLength(1);
    expect(badges[0]?.textContent).toContain("scheduled");
    expect(badges[0]?.textContent).not.toContain("running");
    // cron 区块随 cron 服务挂载（含台账）。
    await waitFor(() =>
      expect(screen.getByTestId("cron-runs-list")).toBeInTheDocument(),
    );
    expect(screen.getByText("*/5 * * * *")).toBeInTheDocument();
  });

  it("triggers a cron service from the overview page after confirm", async () => {
    setToken("flt_test");
    const log: { url: string; method: string }[] = [];
    vi.stubGlobal("fetch", stubOverviewFetch(log));
    renderOverview();
    await waitFor(() => screen.getByTestId("cron-runs-list"));

    fireEvent.click(screen.getByRole("button", { name: "Run now" }));
    fireEvent.click(screen.getByTestId("cron-trigger-button"));

    await waitFor(() =>
      expect(screen.getByTestId("cron-trigger-result").textContent).toContain("triggered"),
    );
    const trigger = log.find(
      (e) => e.method === "POST" && e.url.includes("/services/jobber/trigger"),
    );
    expect(trigger).toBeTruthy();
  });
});

describe("AppOverviewPage platform-admin read-only (P0-3 residual)", () => {
  // /auth/me 包装：owner 成员关系 + is_platform_admin 开关（其余请求透传
  // 给既有分路 stub）。注意 metrics status 的分路：stub 的兜底返回无 mode
  // 字段 → AppMetricsCard 走 opt-in 缺省态；metrics 开关写面 =
  // requirePlatformWriteFace（平台管理员专属）——2026-09-29 前端反转对齐
  // 服务端后，平台管理员见钮、非管理员见说明（资源写钮仍按 P0-3 隐藏）。
  function stubMe(isPlatformAdmin: boolean, inner: ReturnType<typeof stubOverviewFetch>) {
    return vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith("/auth/me")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          statusText: "",
          json: () =>
            Promise.resolve({
              user: { id: "01U1", email: "f@t.test", is_platform_admin: isPlatformAdmin },
              teams: [
                { team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role: "owner" },
              ],
              project_overrides: [],
            }),
        });
      }
      return inner(input, init);
    });
  }

  function renderOverviewInTeamContext(isPlatformAdmin: boolean) {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const inner = stubOverviewFetch();
    vi.stubGlobal("fetch", stubMe(isPlatformAdmin, inner));
    return render(
      <MemoryRouter initialEntries={["/apps/demo"]}>
        <QueryClientProvider client={client}>
          <TeamProjectProvider>
            <Routes>
              <Route path="/apps/:name" element={<AppOverviewPage />} />
            </Routes>
          </TeamProjectProvider>
        </QueryClientProvider>
      </MemoryRouter>,
    );
  }

  it("平台管理员（owner 角色）：cron 触发/扩缩策略等资源写钮隐藏，metrics 开关可见（平台设置写面），读面骨架照常", async () => {
    setToken("flt_test");
    renderOverviewInTeamContext(true);

    // 说明（cron 区块 + 项目网络卡 + 扩缩策略卡 + deploy settings 卡 = 4 处
    // 资源面只读说明；metrics 卡不在内——其写面是平台设置，平台管理员正是
    // 有权方）。
    await waitFor(() => {
      const notes = screen.getAllByTestId("platform-readonly-note");
      expect(notes.length).toBeGreaterThanOrEqual(4);
      expect(notes[0]).toHaveTextContent(
        "Platform administrators have read-only access to resources",
      );
    });
    // 资源写钮不再渲染；metrics 开关（平台全局设置写面）照常可见。
    expect(screen.queryByRole("button", { name: "Run now" })).not.toBeInTheDocument();
    expect(screen.queryByTestId("cron-trigger-button")).not.toBeInTheDocument();
    expect(screen.getByTestId("metrics-mode-toggle")).toBeInTheDocument();
    expect(screen.queryByTestId("metrics-mode-readonly-note")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Add policy" })).not.toBeInTheDocument();
    // 部署入口区（§4.7）：Deploy 卡随能力门关门退场，Redeploy 钮同退。
    expect(screen.queryByTestId("deploy-card")).not.toBeInTheDocument();
    expect(screen.queryByTestId("redeploy-button")).not.toBeInTheDocument();
    // 读面骨架不塌：服务清单 + cron 台账照常。
    await waitFor(() =>
      expect(screen.getByTestId("cron-runs-list")).toBeInTheDocument(),
    );
    expect(screen.getByText("*/5 * * * *")).toBeInTheDocument();
  });

  it("非管理员 owner：资源写钮照常渲染、metrics 卡只见平台管理员说明（零变化防回归）", async () => {
    setToken("flt_test");
    renderOverviewInTeamContext(false);

    await waitFor(() =>
      expect(screen.getByTestId("cron-runs-list")).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: "Run now" })).toBeInTheDocument();
    // metrics 开关对非平台管理员隐藏，原位说明代之（服务端写门同口径）。
    expect(screen.queryByTestId("metrics-mode-toggle")).not.toBeInTheDocument();
    expect(screen.getByTestId("metrics-mode-readonly-note")).toHaveTextContent(
      "Platform administrator required",
    );
    expect(screen.queryByTestId("platform-readonly-note")).not.toBeInTheDocument();
    // 部署入口区装配守卫（§4.7/§5）：owner 见 Deploy settings（Open terminal
    // 入口在卡上；Redeploy/Stop/Start 在详情标题栏 AppHeaderActions——组件
    // 测试在 app-header-actions.test.tsx）+ 单方式 Deploy 卡。
    expect(screen.getByTestId("deploy-settings")).toBeInTheDocument();
    expect(screen.getByTestId("deploy-card")).toBeInTheDocument();
  });
});

describe("AppOverviewPage time display (2026-09-25 review P2-3)", () => {
  it("renders Created/Updated as relative time with the absolute value in the title", async () => {
    setToken("flt_test");
    const created = new Date(Date.now() - 3 * 3_600_000).toISOString();
    const updated = new Date(Date.now() - 30_000).toISOString();
    vi.stubGlobal("fetch", stubOverviewFetch([], { created_at: created, updated_at: updated }));

    renderOverview();

    // Created 与 Updated 同为相对时间（此前 Created=绝对、Updated=相对并存），
    // 绝对值留 title tooltip。
    const createdEl = await screen.findByTitle(created);
    expect(createdEl.textContent).toBe("3h ago");
    const updatedEl = screen.getByTitle(updated);
    expect(updatedEl.textContent).toBe("30s ago");
    expect(screen.getByText("Created")).toBeInTheDocument();
  });
});

describe("AppOverviewPage Danger Zone (backlog #4-①)", () => {
  // /auth/me 包装 + 团队上下文渲染（P0-3 describe 的 helper 在其作用域内，
  // 本 describe 自备同形态装配）。
  function stubMe(isPlatformAdmin: boolean, inner: ReturnType<typeof stubOverviewFetch>) {
    return vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).endsWith("/auth/me")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          statusText: "",
          json: () =>
            Promise.resolve({
              user: { id: "01U1", email: "f@t.test", is_platform_admin: isPlatformAdmin },
              teams: [
                { team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role: "owner" },
              ],
              project_overrides: [],
            }),
        });
      }
      return inner(input, init);
    });
  }

  function renderInTeamContext(isPlatformAdmin: boolean) {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    const inner = stubOverviewFetch();
    vi.stubGlobal("fetch", stubMe(isPlatformAdmin, inner));
    return render(
      <MemoryRouter initialEntries={["/apps/demo"]}>
        <QueryClientProvider client={client}>
          <TeamProjectProvider>
            <Routes>
              <Route path="/apps/:name" element={<AppOverviewPage />} />
            </Routes>
          </TeamProjectProvider>
        </QueryClientProvider>
      </MemoryRouter>,
    );
  }

  it("平台管理员：Danger Zone 无删除钮，原位只读说明（资源面双门）", async () => {
    setToken("flt_test");
    renderInTeamContext(true);

    await screen.findByTestId("danger-zone");
    // me 投影落地前能力门 fail-open（体验门缺省全开——context.tsx 口径），
    // 稳态以只读说明渲染为准；随后断言删除钮已消失。
    await waitFor(() => {
      const notes = screen.getAllByTestId("platform-readonly-note");
      expect(
        notes.some((n) => n.textContent?.includes("Delete applications from the CLI")),
      ).toBe(true);
    });
    expect(screen.queryByTestId("app-delete-button")).not.toBeInTheDocument();
  });

  it("owner：两步确认（输名前禁用）→ DELETE /apps/demo → 导航回 /apps", async () => {
    setToken("flt_test");
    const log: { url: string; method: string }[] = [];
    // 团队上下文 + /apps 探针路由（导航断言）。log 经 stubOverviewFetch 传入。
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    vi.stubGlobal(
      "fetch",
      stubMe(false, stubOverviewFetch(log)),
    );
    render(
      <MemoryRouter initialEntries={["/apps/demo"]}>
        <QueryClientProvider client={client}>
          <TeamProjectProvider>
            <Routes>
              <Route path="/apps/:name" element={<AppOverviewPage />} />
              <Route path="/apps" element={<div data-testid="apps-page-probe" />} />
            </Routes>
          </TeamProjectProvider>
        </QueryClientProvider>
      </MemoryRouter>,
    );

    await screen.findByTestId("danger-zone");
    fireEvent.click(screen.getByTestId("app-delete-button"));

    // 两步确认：对话框弹出且未输名前提交禁用。
    const dialog = await screen.findByTestId("app-delete-dialog");
    expect((screen.getByTestId("app-delete-submit") as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByTestId("app-delete-confirm-input"), {
      target: { value: "demo" },
    });
    expect((screen.getByTestId("app-delete-submit") as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(screen.getByTestId("app-delete-submit"));
    void dialog;

    // 删除成功：DELETE 载荷落地 + 导航回 /apps。
    await waitFor(() =>
      expect(screen.getByTestId("apps-page-probe")).toBeInTheDocument(),
    );
    const del = log.find(
      (e) => e.method === "DELETE" && e.url.endsWith("/apps/demo"),
    );
    expect(del).toBeTruthy();
  });
});
