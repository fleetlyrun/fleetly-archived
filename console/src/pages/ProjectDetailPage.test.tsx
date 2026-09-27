// 项目详情页测试（2026-09-25「项目怎么查看或编辑详情」验收面）：信息渲染
// （slug/name/description/限定形）、owner 编辑态（PATCH 载荷 name+description，
// slug 不出现在载荷——不可变）、非 owner 只读（无编辑钮）、项目内资源清单
// （apps/dbs 按 ?project= 收窄）。P0-1 增补：创建 CTA 角色门（developer 可见
// deploy / admin 可见建库 / viewer 不可见 / 平台管理员说明态）、对话框目标
// 项目展示、deploy 载荷端到端断言。ProjectsPage.test 同款 mock 形态。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { ProjectDetailPage } from "@/pages/ProjectDetailPage";
import { TeamProjectProvider } from "@/lib/context";

const PROJECT = {
  project: {
    id: "01PRJ1",
    team_id: "01T1",
    team_slug: "founder",
    slug: "staging",
    name: "Staging",
    description: "pre-prod fleet",
    created_at: "2026-09-24T13:58:59Z",
    // 项目网投影（IMPL-T15-1/OT-1）：overlay 名 + 参与成员数。
    network_name: "fleetly-project-01PRJ1",
    network_members: 1,
  },
};

const COMPOSE_NO_NAME = "services:\n  web:\n    image: nginx:1.27-alpine";

function stubFetch(opts: {
  role?: string;
  project?: typeof PROJECT;
  platformAdmin?: boolean;
  apps?: unknown[];
  databases?: unknown[];
} = {}) {
  const calls: { url: string; method: string; body: Record<string, unknown> }[] = [];
  const fetchMock = vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (url.includes("/auth/me")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({
            user: { id: "01U1", email: "f@t.test", is_platform_admin: opts.platformAdmin ?? false },
            teams: [{ team_id: "01T1", team_slug: "founder", team_name: "Founder", role: opts.role ?? "owner" }],
          }),
      });
    }
    // 创建应用对话框的 deploy 入队（先于 /apps 列表分支匹配——URL 同时含两者）。
    if (method === "POST" && url.includes("/deployments")) {
      calls.push({ url, method, body: JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown> });
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ deployment_id: "DEP9", app: "web-api", status: "queued" }),
      });
    }
    if (url.includes("/apps")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ apps: "apps" in opts ? opts.apps : [{ id: "A1", name: "demo", derived_state: "running", project_network_attached: true }] }),
      });
    }
    if (url.includes("/databases")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ databases: "databases" in opts ? opts.databases : [{ id: "D1", name: "pgshared", template: "postgres", status: "ready" }] }),
      });
    }
    if (url.endsWith("/v1/projects/01PRJ1") && method === "PATCH") {
      const body = JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown>;
      calls.push({ url, method, body });
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ project: { ...PROJECT.project, ...body } }) });
    }
    if (url.includes("/projects/01PRJ1")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve(opts.project ?? PROJECT) });
    }
    return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({}) });
  });
  return { fetchMock, calls };
}

function renderAt() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter initialEntries={["/projects/01PRJ1"]}>
      <QueryClientProvider client={client}>
        <TeamProjectProvider>
          <Routes>
            <Route path="/projects/:projectId" element={<ProjectDetailPage />} />
            <Route path="/teams/:teamId" element={<p>team-settings</p>} />
            <Route path="/apps/:name" element={<p>app-detail</p>} />
            {/* 创建应用成功后的落地路由（导航断言/消除 no-match 噪音）。 */}
            <Route path="/apps" element={<p>apps-landing</p>} />
          </Routes>
        </TeamProjectProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("ProjectDetailPage", () => {
  it("renders project info with qualified name and in-project resources", async () => {
    vi.stubGlobal("fetch", stubFetch().fetchMock);
    renderAt();

    expect(await screen.findByTestId("project-detail-name")).toHaveTextContent("Staging");
    expect(screen.getByText("founder/staging")).toBeInTheDocument();
    expect(screen.getByTestId("project-detail-description")).toHaveTextContent("pre-prod fleet");
    // 项目网面（IMPL-T15-1/OT-1）：overlay 名 + 参与成员数（信息卡）；
    // 应用行带参与徽标（attach/detach 操作面在 app 概览卡）。
    expect(screen.getByTestId("project-network-name")).toHaveTextContent(
      "fleetly-project-01PRJ1",
    );
    expect(screen.getByTestId("project-network-summary")).toHaveTextContent("1 app attached");
    await waitFor(() =>
      expect(screen.getByTestId("project-app-network-badge")).toHaveTextContent("attached"),
    );
    // 项目内资源清单（收窄断言：URL 带 project=founder/staging）。
    await waitFor(() => expect(screen.getByTestId("project-app-row")).toBeInTheDocument());
    expect(screen.getByTestId("project-db-row")).toBeInTheDocument();
    const appsCall = (globalThis.fetch as ReturnType<typeof vi.fn>).mock.calls.find((c) =>
      String(c[0]).includes("/v1/apps?"),
    );
    expect(String(appsCall?.[0])).toContain("project=founder%2Fstaging");
  });

  it("edits name and description as owner (PATCH without slug)", async () => {
    const { fetchMock, calls } = stubFetch();
    vi.stubGlobal("fetch", fetchMock);
    renderAt();
    const user = userEvent.setup();

    await screen.findByTestId("project-edit-open");
    await user.click(screen.getByTestId("project-edit-open"));
    await waitFor(() => expect(screen.getByTestId("project-name-input")).toHaveValue("Staging"));
    await user.clear(screen.getByTestId("project-name-input"));
    await user.type(screen.getByTestId("project-name-input"), "Staging v2");
    await user.click(screen.getByTestId("project-save"));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].method).toBe("PATCH");
    expect(calls[0].body).toEqual({ name: "Staging v2", description: "pre-prod fleet" });
    expect(calls[0].body).not.toHaveProperty("slug");
  });

  it("hides the edit affordance for non-owner members", async () => {
    vi.stubGlobal("fetch", stubFetch({ role: "developer" }).fetchMock);
    renderAt();

    expect(await screen.findByTestId("project-detail-name")).toBeInTheDocument();
    expect(screen.queryByTestId("project-edit-open")).not.toBeInTheDocument();
  });

  it("points the two header links at distinct team tabs (?tab=members / ?tab=projects)", async () => {
    vi.stubGlobal("fetch", stubFetch().fetchMock);
    renderAt();

    await screen.findByTestId("project-detail-name");
    // team settings → 成员管理 tab；project role overrides → 覆写管理面
    //（团队设置 Projects tab，单一管理面）。此前两链接同指 /teams/:id；
    // 文案直说落点语义（2026-09-25 走查：'members & role overrides' 含糊）。
    expect(screen.getByRole("link", { name: "team settings" }).getAttribute("href")).toBe(
      "/teams/01T1?tab=members",
    );
    expect(
      screen.getByRole("link", { name: "project role overrides" }).getAttribute("href"),
    ).toBe("/teams/01T1?tab=projects");
  });
});

/** 与 client.utf8ToBase64 对称的解码（断言上行 compose 原文）。 */
function decodeBase64(b64: string): string {
  const bin = atob(b64);
  const bytes = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
  return new TextDecoder().decode(bytes);
}

describe("ProjectDetailPage create CTAs", () => {
  it("shows both CTAs for owner and displays the target project in dialogs", async () => {
    vi.stubGlobal("fetch", stubFetch().fetchMock);
    renderAt();
    const user = userEvent.setup();

    await screen.findByTestId("project-app-row");
    expect(screen.getByTestId("project-app-create-button")).toBeInTheDocument();
    expect(screen.getByTestId("project-db-create-button")).toBeInTheDocument();

    await user.click(screen.getByTestId("project-app-create-button"));
    expect(screen.getByTestId("app-create-dialog")).toBeInTheDocument();
    expect(screen.getByTestId("app-target-project")).toHaveTextContent("founder/staging");

    // 关掉应用对话框（模态遮罩会让卡片头按钮 pointer-events:none）再开建库
    // 对话框——两个对话框断言各自由。
    await user.keyboard("{Escape}");
    await waitFor(() =>
      expect(screen.queryByTestId("app-create-dialog")).not.toBeInTheDocument(),
    );

    await user.click(screen.getByTestId("project-db-create-button"));
    expect(screen.getByTestId("database-create-dialog")).toBeInTheDocument();
    expect(screen.getByTestId("database-target-project")).toHaveTextContent("founder/staging");
  });

  it("deploys a new app from the dialog with app path, project and injected compose name", async () => {
    const { fetchMock, calls } = stubFetch();
    vi.stubGlobal("fetch", fetchMock);
    renderAt();
    const user = userEvent.setup();

    await screen.findByTestId("project-app-row");
    await user.click(screen.getByTestId("project-app-create-button"));
    await user.type(screen.getByTestId("app-create-name-input"), "web-api");
    await user.type(screen.getByTestId("app-create-compose-input"), COMPOSE_NO_NAME);
    await user.click(screen.getByTestId("app-create-submit"));

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0].url).toContain("/v1/apps/web-api/deployments");
    expect(calls[0].body.project).toBe("founder/staging");
    // compose 无顶层 name → `name: web-api` 前置注入（服务端 A1 一致性）。
    expect(decodeBase64(String(calls[0].body.compose))).toBe(
      `name: web-api\n${COMPOSE_NO_NAME}`,
    );
  });

  it("gates CTAs by role: developer sees the deploy CTA only", async () => {
    vi.stubGlobal("fetch", stubFetch({ role: "developer" }).fetchMock);
    renderAt();

    expect(await screen.findByTestId("project-app-create-button")).toBeInTheDocument();
    expect(screen.queryByTestId("project-db-create-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("platform-readonly-note")).not.toBeInTheDocument();
  });

  it("shows no CTAs for a viewer", async () => {
    vi.stubGlobal("fetch", stubFetch({ role: "viewer" }).fetchMock);
    renderAt();

    expect(await screen.findByTestId("project-app-row")).toBeInTheDocument();
    expect(screen.queryByTestId("project-app-create-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("project-db-create-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("platform-readonly-note")).not.toBeInTheDocument();
  });

  it("shows the platform-readonly note instead of CTAs for a platform admin", async () => {
    vi.stubGlobal("fetch", stubFetch({ platformAdmin: true }).fetchMock);
    renderAt();

    expect(await screen.findByTestId("platform-readonly-note")).toBeInTheDocument();
    expect(screen.queryByTestId("project-app-create-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("project-db-create-button")).not.toBeInTheDocument();
  });

  it("renders empty-state CTAs when the project has no resources", async () => {
    vi.stubGlobal("fetch", stubFetch({ apps: [], databases: [] }).fetchMock);
    renderAt();
    const user = userEvent.setup();

    expect(await screen.findByText("No applications in this project yet.")).toBeInTheDocument();
    expect(screen.getByText("No databases in this project yet.")).toBeInTheDocument();
    expect(screen.getByTestId("project-app-empty-cta")).toBeInTheDocument();
    expect(screen.getByTestId("project-db-empty-cta")).toBeInTheDocument();

    await user.click(screen.getByTestId("project-app-empty-cta"));
    expect(screen.getByTestId("app-create-dialog")).toBeInTheDocument();
  });
});
