// Overview 页部署入口区测试（2026-09-29 §4.7 三轮重设计：部署入口自
// Deployments 页上移首页签——单方式 Deploy 卡随迁 + 新增 Deploy settings
// 快捷动作卡）。覆盖：
//   - Deploy 卡方式 pills / 触发面（git/webhook pane 全部触发面 testid 随
//     组件迁入，锚点不变）；方式选择按应用 localStorage 记忆；角色门投影
//     （developer 只见 Compose；平台管理员整卡不渲染、说明由 Deploy
//     settings 卡承载）。
//   - Compose 部署跟踪（M9-5/M9-7）：轮询持续失败的错误信封一等渲染。
//   - 项目归属上下文（2026-09-25 走查 + W2-4）：Deploy 载荷携带应用自身
//     归属 team/prj；多团队未选禁用+指路；单团队缺省不带 project。
//   - Deploy settings 卡：Redeploy = POST /rollbacks 携带 active revision；
//     无 active revision 禁用+指路；Open terminal 链接指向 Terminal 页签。
//
// 注意：Me 投影落地前能力门 fail-open（体验门缺省全开），角色相关断言前
// 先等稳态信号（triggers-admin-note / platform-readonly-note）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppOverviewPage } from "@/pages/AppOverviewPage";
import { TeamProjectProvider } from "@/lib/context";
import { setToken } from "@/api/client";

beforeEach(() => {
  // 部署方式选择按应用持久化在 localStorage——跨用例清场防泄漏。
  window.localStorage.clear();
});

function ok(body: unknown) {
  return {
    ok: true,
    status: 200,
    statusText: "",
    json: () => Promise.resolve(body),
  };
}

// ── Compose 部署跟踪（M9-5 / M9-7）───────────────────────────────────────

describe("AppOverviewPage deployment tracking (M9-5 / M9-7)", () => {
  it("renders the error envelope when the tracked deployment poll keeps failing (no infinite spinner)", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((url: string, init?: { method?: string }) => {
        const u = String(url);
        // Deploy 提交成功入队 → 开始跟踪。
        if (init?.method === "POST" && u.includes("/apps/demo/deployments")) {
          return Promise.resolve(
            ok({ deployment_id: "dep_track", warnings: [] }),
          );
        }
        // 跟踪轮询持续 500（空 data 形态——refetchInterval 回调不得抛
        // TypeError，页面不得永远转圈）。
        if (u.includes("/deployments/dep_track")) {
          return Promise.resolve({
            ok: false,
            status: 500,
            statusText: "",
            json: () =>
              Promise.resolve({
                code: "E_INTERNAL",
                message: "tracking unavailable",
                suggestion: "check daemon logs",
              }),
          });
        }
        return Promise.resolve(ok({ deployments: [], revisions: [] }));
      }),
    );

    renderOverviewPage();
    const user = userEvent.setup();
    await user.type(
      screen.getByLabelText("Compose YAML"),
      "services:\n  web:\n    image: nginx:1.27-alpine\n",
    );
    await user.click(screen.getByRole("button", { name: "Deploy" }));

    await waitFor(() => {
      const envelope = screen.getByTestId("error-envelope");
      expect(envelope).toHaveTextContent("E_INTERNAL");
      expect(envelope).toHaveTextContent("tracking unavailable");
      expect(envelope).toHaveTextContent("check daemon logs");
    });
    // 跟踪块仍在（部署 ID 可见），只是状态以错误信封呈现。
    expect(screen.getByTestId("deployment-tracker")).toHaveTextContent(
      "dep_track",
    );
  });
});

function renderOverviewPage(
  initialEntry = "/apps/demo",
  withTeamContext = false,
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const page = (
    <MemoryRouter initialEntries={[initialEntry]}>
      <QueryClientProvider client={client}>
        {withTeamContext ? (
          <TeamProjectProvider>
            <Routes>
              <Route path="/apps/:name" element={<AppOverviewPage />} />
            </Routes>
          </TeamProjectProvider>
        ) : (
          <Routes>
            <Route path="/apps/:name" element={<AppOverviewPage />} />
          </Routes>
        )}
      </QueryClientProvider>
    </MemoryRouter>
  );
  return render(page);
}

// ── Deploy triggers（P1-8「Git push 通道不可发现」；随 Deploy 卡迁入）──────

// 26 字符规范 ULID：Console 详情导航以平台 id 寻址（路由参数 = 平台 id，
// 非业务名）——触发卡测试挂 id 路由，钉「接收端 URL 用业务名而非 id」。
const APP_REF = "01ARZ3NDEKTSV4RRFFQ69G5FAV";

const WEBHOOK_CFG = {
  name: "demo",
  secret_configured: true,
  source_url: "https://git.example.com/acme/demo.git",
  source_branch: "release",
  source_auth_kind: "https_token",
};

/** 触发面全 stub：Me（角色开关）+ webhook 读面 + 写面回显 + Overview 其余
 * 查询兜底（revisions 空集→Deploy settings 卡无 active revision 形态）。 */
function stubTriggers(role: string, isPlatformAdmin: boolean, cfg: unknown) {
  const calls: Array<{ url: string; method?: string; body?: unknown }> = [];
  const fetchMock = vi.fn().mockImplementation((url: string, init?: { method?: string; body?: string }) => {
    const u = String(url);
    calls.push({ url: u, method: init?.method, body: init?.body ? JSON.parse(init.body) : undefined });
    if (u.endsWith("/auth/me")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({
            user: { id: "01U1", email: "f@t.test", is_platform_admin: isPlatformAdmin },
            teams: [{ team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role }],
            project_overrides: [],
          }),
      });
    }
    if (u.endsWith("/webhook") && (!init?.method || init.method === "GET")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve(cfg) });
    }
    if (init?.method === "PUT" && u.endsWith("/webhook-secret")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ name: "demo", configured: true }) });
    }
    if (init?.method === "PUT" && u.endsWith("/source")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ name: "demo" }) });
    }
    if (u.endsWith("/revisions")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ revisions: [] }) });
    }
    // GET /apps/{ref}（GetAppResponse）与其余查询兜底应用形态。
    return Promise.resolve({
      ok: true, status: 200, statusText: "",
      json: () => Promise.resolve({ id: "a1", name: "demo", lifecycle: "active", derived_state: "running" }),
    });
  });
  return { fetchMock, calls };
}

function renderTriggersPage() {
  return renderOverviewPage(`/apps/${APP_REF}`, true);
}

describe("AppOverviewPage deploy triggers (P1-8)", () => {
  it("admin+ read state: fetch source fields, trigger branch, webhook URL and secret badge", async () => {
    setToken("flt_test");
    const { fetchMock } = stubTriggers("owner", false, WEBHOOK_CFG);
    vi.stubGlobal("fetch", fetchMock);
    renderTriggersPage();

    // admin+（owner）见 Deploy 卡，三种方式 pill 齐备；默认 Compose。
    await waitFor(() => expect(screen.getByTestId("deploy-card")).toBeInTheDocument());
    await waitFor(() =>
      expect(screen.getByLabelText("Compose YAML")).toBeInTheDocument(),
    );

    // 切到 Git 方式：读态字段全部来自 ShowAppWebhook 响应（异步到达——以
    // 拉源状态行为就绪信号）。
    fireEvent.click(screen.getByRole("tab", { name: "Git" }));
    await waitFor(() =>
      expect(screen.getByTestId("triggers-source-state")).toHaveTextContent("https://git.example.com/acme/demo.git"),
    );
    expect(screen.getByTestId("triggers-branch")).toHaveTextContent("release");

    // 切到 Webhook 方式：接收端 URL（gateway 既有路由拼装）——路径段用
    // 响应的业务名（name 字段）而非路由参数（平台 id）：id 形态永不匹配
    // 服务端接收端分派正则，拼进去就是恒 404 死链。
    fireEvent.click(screen.getByRole("tab", { name: "Webhook" }));
    await waitFor(() =>
      expect(screen.getByTestId("triggers-webhook-url")).toHaveTextContent("/v1/apps/demo/webhooks/github"),
    );
    expect(screen.getByTestId("triggers-webhook-url").textContent).not.toContain(APP_REF);
    expect(screen.getByTestId("triggers-secret-configured")).toHaveTextContent("Configured (never displayed)");

    // 切回 Git 验证方式记忆（pane 内容随 pill 走；push remote 行已随
    // git push 面移除——Git pane 即拉源配置）。
    fireEvent.click(screen.getByRole("tab", { name: "Git" }));
    await waitFor(() =>
      expect(screen.getByTestId("triggers-source-state")).toBeInTheDocument(),
    );
  });

  it("remembers the chosen deploy method per app (localStorage)", async () => {
    setToken("flt_test");
    const { fetchMock } = stubTriggers("owner", false, WEBHOOK_CFG);
    vi.stubGlobal("fetch", fetchMock);
    const { unmount } = renderTriggersPage();

    await waitFor(() => expect(screen.getByTestId("deploy-card")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("tab", { name: "Git" }));
    await waitFor(() =>
      expect(screen.getByTestId("deploy-git-pane")).toBeInTheDocument(),
    );
    unmount();

    // 重挂载：方式记忆生效——直接落在 Git pane（不再回 Compose 缺省）。
    renderTriggersPage();
    await waitFor(() =>
      expect(screen.getByTestId("deploy-git-pane")).toBeInTheDocument(),
    );
    expect(screen.queryByLabelText("Compose YAML")).not.toBeInTheDocument();
  });

  it("discloses honestly when the app name cannot match the receiver path pattern", async () => {
    setToken("flt_test");
    // 下划线名：服务端接收端分派正则（[a-z0-9][a-z0-9-]{0,62}）不收——
    // URL 照拼（如实展示），pane 内出说明（服务端限制），不静默冒充可用。
    const { fetchMock } = stubTriggers("owner", false, { ...WEBHOOK_CFG, name: "demo_app" });
    vi.stubGlobal("fetch", fetchMock);
    renderTriggersPage();

    await waitFor(() => expect(screen.getByTestId("deploy-card")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("tab", { name: "Webhook" }));
    await waitFor(() =>
      expect(screen.getByTestId("triggers-webhook-url")).toHaveTextContent(
        "/v1/apps/demo_app/webhooks/github",
      ),
    );
    expect(screen.getByTestId("triggers-webhook-name-note")).toHaveTextContent(
      "server-side",
    );
  });

  it("secret rotation: PUT payload carries the value, success never echoes the plaintext", async () => {
    setToken("flt_test");
    // secret 未配置态起手（缺 secret_configured 位）。
    const { fetchMock, calls } = stubTriggers("owner", false, { name: "demo" });
    vi.stubGlobal("fetch", fetchMock);
    renderTriggersPage();
    const user = userEvent.setup();

    await waitFor(() => expect(screen.getByTestId("deploy-card")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("tab", { name: "Webhook" }));
    await waitFor(() => expect(screen.getByTestId("triggers-secret-missing")).toBeInTheDocument());

    const secretValue = "whsec-0123456789abcdef";
    await user.type(screen.getByTestId("triggers-secret-input"), secretValue);
    await user.click(screen.getByTestId("triggers-secret-open"));
    expect(screen.getByTestId("triggers-secret-dialog")).toHaveTextContent("takes effect immediately");

    await user.click(screen.getByTestId("triggers-secret-submit"));
    await waitFor(() => {
      const put = calls.find((c) => c.method === "PUT" && c.url.endsWith("/webhook-secret"));
      expect(put?.url.endsWith(`/v1/apps/${APP_REF}/webhook-secret`)).toBe(true);
      expect(put?.body).toEqual({ secret: secretValue });
    });

    // 不回显断言：成功提示出现、确认框关闭、明文在文档任何位置都不存在。
    await waitFor(() => expect(screen.getByTestId("triggers-secret-stored")).toBeInTheDocument());
    expect(screen.queryByTestId("triggers-secret-dialog")).not.toBeInTheDocument();
    expect(screen.queryByText(secretValue)).not.toBeInTheDocument();
    // 输入复位（type=password 输入框同样不得残留明文）。
    expect(screen.getByTestId("triggers-secret-input")).toHaveValue("");
  });

  it("set source: PUT payload is the whole replacement (url+branch+kind; no secret key for none)", async () => {
    setToken("flt_test");
    const { fetchMock, calls } = stubTriggers("owner", false, { name: "demo" });
    vi.stubGlobal("fetch", fetchMock);
    renderTriggersPage();
    const user = userEvent.setup();

    await waitFor(() => expect(screen.getByTestId("deploy-card")).toBeInTheDocument());
    fireEvent.click(screen.getByRole("tab", { name: "Git" }));
    await waitFor(() =>
      expect(screen.getByTestId("source-branch-input")).toBeInTheDocument(),
    );
    // 未水合到 source 字段时分支缺省 main（proto source_branch 默认语义）。
    expect(screen.getByTestId("source-branch-input")).toHaveValue("main");

    await user.type(screen.getByTestId("source-url-input"), "https://git.example.com/acme/demo.git");
    await user.click(screen.getByTestId("source-submit"));

    await waitFor(() => {
      const put = calls.find((c) => c.method === "PUT" && c.url.endsWith("/source"));
      expect(put?.url.endsWith(`/v1/apps/${APP_REF}/source`)).toBe(true);
      expect(put?.body).toEqual({
        source_url: "https://git.example.com/acme/demo.git",
        source_branch: "main",
        source_auth_kind: "none",
      });
    });
    expect(screen.getByTestId("source-stored")).toBeInTheDocument();
  });

  it("role gate: pills project by role — developer sees Compose only, platform admin sees the readonly note", async () => {
    setToken("flt_test");
    const { fetchMock: adminFetch } = stubTriggers("owner", false, WEBHOOK_CFG);
    vi.stubGlobal("fetch", adminFetch);
    const { unmount } = renderTriggersPage();
    await waitFor(() => expect(screen.getByTestId("deploy-card")).toBeInTheDocument());
    // admin+（owner）：三种方式 pill 齐备。
    expect(screen.getByRole("tab", { name: "Compose" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Git" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Webhook" })).toBeInTheDocument();
    unmount();

    // developer（canDeploy ✓ / canAdminResources ✗）：只有 Compose pill，
    // 触发面以卡内说明态如实披露（不静默消失）。说明态出现 = Me 投影已
    // 生效（能力门 fail-open 窗口结束）——以它为稳态信号再断言 pill 收窄。
    const { fetchMock: devFetch } = stubTriggers("developer", false, WEBHOOK_CFG);
    vi.stubGlobal("fetch", devFetch);
    const dev = renderTriggersPage();
    await waitFor(() => expect(screen.getByTestId("deploy-card")).toBeInTheDocument());
    await waitFor(() =>
      expect(screen.getByTestId("triggers-admin-note")).toBeInTheDocument(),
    );
    expect(screen.getByRole("tab", { name: "Compose" })).toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Git" })).not.toBeInTheDocument();
    expect(screen.queryByRole("tab", { name: "Webhook" })).not.toBeInTheDocument();
    dev.unmount();

    // 平台管理员：能力门统一关门 → Deploy 卡不渲染；Deploy settings 卡
    // 原位出只读说明（P0-3 双门；其余卡也各有说明——断言「存在即可」）。
    const { fetchMock: paFetch } = stubTriggers("owner", true, WEBHOOK_CFG);
    vi.stubGlobal("fetch", paFetch);
    renderTriggersPage();
    await waitFor(() => {
      const notes = screen.getAllByTestId("platform-readonly-note");
      expect(notes.some((n) => n.textContent?.includes("Platform administrators have read-only access"))).toBe(true);
    });
    expect(screen.queryByTestId("deploy-card")).not.toBeInTheDocument();
    expect(screen.queryByTestId("redeploy-button")).not.toBeInTheDocument();
  });
});

// ── DeployCard 项目归属上下文（2026-09-25 走查 + W2-4 二修）────────────────
// 多团队成员从 Deploy 卡重部署 → deploy() 未传 project → 服务端缺省解析
// 不到归属项目 → 400 no default project resolvable。修复：应用自身归属
//（GetApp 响应 team_slug/project_slug）优先随载荷携带 `team/prj`；多团队
// 未选且归属未达禁用 Deploy 并卡内指路；单团队用户缺省（服务端回落个人
// 队 default）——载荷形态不变防回归。
describe("AppOverviewPage DeployCard project context", () => {
  const TEAMS = [
    { team_id: "01TA", team_slug: "acme", team_name: "Acme", role: "admin" },
    { team_id: "01TB", team_slug: "globex", team_name: "Globex", role: "owner" },
  ];
  const PROJECTS = {
    projects: [
      { id: "01PA", team_id: "01TA", team_slug: "acme", slug: "staging", name: "Staging" },
    ],
  };

  /** 多团队 stub：Me + /projects + 部署历史 + Deploy POST 捕获。带
   * resolveAppAttribution 开关时 /apps/demo GET 返回归属（W2-4）。 */
  function stubMultiTeam(opts: { appAttribution?: boolean } = {}) {
    const calls: Array<{ url: string; method?: string; body?: unknown }> = [];
    const fetchMock = vi.fn().mockImplementation((url: string, init?: { method?: string; body?: string }) => {
      const u = String(url);
      calls.push({ url: u, method: init?.method, body: init?.body ? JSON.parse(init.body) : undefined });
      if (u.endsWith("/auth/me")) {
        return Promise.resolve({
          ok: true, status: 200, statusText: "",
          json: () =>
            Promise.resolve({
              user: { id: "01U1", email: "f@t.test", is_platform_admin: false },
              teams: TEAMS,
              project_overrides: [],
            }),
        });
      }
      if (u.endsWith("/projects")) {
        return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve(PROJECTS) });
      }
      if (opts.appAttribution && u.endsWith("/apps/demo") && (!init?.method || init.method === "GET")) {
        // W2-4：详情壳的 GetApp 响应带应用归属 slug（Deploy 卡优先消费）。
        return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ name: "demo", team_slug: "acme", project_slug: "staging" }) });
      }
      if (init?.method === "POST" && u.includes("/apps/demo/deployments")) {
        return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployment_id: "dep_ctx", warnings: [] }) });
      }
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ revisions: [] }) });
    });
    return { fetchMock, calls };
  }

  function renderWithContext() {
    return renderOverviewPage("/apps/demo", true);
  }

  it("多团队+已选上下文：Deploy 载荷携带 team/prj（修复 400 no default project）", async () => {
    setToken("flt_test");
    window.localStorage.setItem(
      "fleetly.console.context",
      JSON.stringify({ team: "acme", project: "staging" }),
    );
    const { fetchMock, calls } = stubMultiTeam();
    vi.stubGlobal("fetch", fetchMock);

    renderWithContext();
    await waitFor(() =>
      expect(screen.getByLabelText("Compose YAML")).toBeInTheDocument(),
    );
    const user = userEvent.setup();
    await user.type(
      screen.getByLabelText("Compose YAML"),
      "services:\n  web:\n    image: nginx:1.27-alpine\n",
    );
    await user.click(screen.getByRole("button", { name: "Deploy" }));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "POST" && c.url.includes("/apps/demo/deployments"));
      expect(post).toBeTruthy();
      // project 以 team/prj 限定形随载荷上行（deployments.proto body:"*"）。
      expect(post?.body).toHaveProperty("project", "acme/staging");
    });
    expect(screen.getByTestId("deployment-tracker")).toHaveTextContent("dep_ctx");
  });

  it("多团队未选项目 + 应用归属可解析（W2-4）：Deploy 启用，载荷携带应用自身归属", async () => {
    setToken("flt_test");
    // 只选团队未选项目：顶栏上下文为空，但应用自身归属（GetApp 的
    // team_slug/project_slug）已返回 → 优先消费，Deploy 不再被全局上下文
    // 卡住（隐式全局状态依赖是缺口本身）。
    window.localStorage.setItem(
      "fleetly.console.context",
      JSON.stringify({ team: "acme", project: null }),
    );
    const { fetchMock, calls } = stubMultiTeam({ appAttribution: true });
    vi.stubGlobal("fetch", fetchMock);

    renderWithContext();
    await waitFor(() =>
      expect(screen.getByLabelText("Compose YAML")).toBeInTheDocument(),
    );
    const user = userEvent.setup();
    await user.type(
      screen.getByLabelText("Compose YAML"),
      "services:\n  web:\n    image: nginx:1.27-alpine\n",
    );
    // 等待应用归属解析完成（按钮从禁用翻转为可用）。
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Deploy" })).toBeEnabled(),
    );
    await user.click(screen.getByRole("button", { name: "Deploy" }));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "POST" && c.url.includes("/apps/demo/deployments"));
      expect(post).toBeTruthy();
      // 载荷 project = 应用自身归属（acme/staging），与顶栏选择无关。
      expect(post?.body).toHaveProperty("project", "acme/staging");
    });
  });

  it("多团队未选项目 + 应用归属未达：Deploy 禁用 + 卡内指路提示（不发请求）", async () => {
    setToken("flt_test");
    // 应用归属未返回（stub 无 /apps/demo GET）且顶栏未选 → 保持旧门：
    // 禁用 + 指路，不静默发缺省部署。
    window.localStorage.setItem(
      "fleetly.console.context",
      JSON.stringify({ team: "acme", project: null }),
    );
    const { fetchMock, calls } = stubMultiTeam();
    vi.stubGlobal("fetch", fetchMock);

    renderWithContext();
    await waitFor(() =>
      expect(screen.getByLabelText("Compose YAML")).toBeInTheDocument(),
    );
    const user = userEvent.setup();
    await user.type(
      screen.getByLabelText("Compose YAML"),
      "services:\n  web:\n    image: nginx:1.27-alpine\n",
    );

    const deployButton = screen.getByRole("button", { name: "Deploy" });
    expect(deployButton).toBeDisabled();
    expect(screen.getByTestId("deploy-project-context-hint")).toHaveTextContent(
      "Select a project above to deploy",
    );
    // 有 compose 内容也不得发出 Deploy POST（按钮禁用即无请求面）。
    expect(
      calls.some((c) => c.method === "POST" && c.url.includes("/apps/demo/deployments")),
    ).toBe(false);
  });

  it("单团队回归：未选上下文 Deploy 载荷不带 project（服务端个人队缺省）", async () => {
    setToken("flt_test");
    const calls: Array<{ url: string; method?: string; body?: unknown }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((url: string, init?: { method?: string; body?: string }) => {
        const u = String(url);
        calls.push({ url: u, method: init?.method, body: init?.body ? JSON.parse(init.body) : undefined });
        if (u.endsWith("/auth/me")) {
          return Promise.resolve({
            ok: true, status: 200, statusText: "",
            json: () =>
              Promise.resolve({
                user: { id: "01U1", email: "f@t.test", is_platform_admin: false },
                teams: [TEAMS[0]],
                project_overrides: [],
              }),
          });
        }
        if (u.endsWith("/projects")) {
          return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve(PROJECTS) });
        }
        if (init?.method === "POST" && u.includes("/apps/demo/deployments")) {
          return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployment_id: "dep_solo", warnings: [] }) });
        }
        return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ revisions: [] }) });
      }),
    );

    renderWithContext();
    await waitFor(() =>
      expect(screen.getByLabelText("Compose YAML")).toBeInTheDocument(),
    );
    const user = userEvent.setup();
    await user.type(
      screen.getByLabelText("Compose YAML"),
      "services:\n  web:\n    image: nginx:1.27-alpine\n",
    );
    // 单团队用户零选择也有正确能力（useTeamCapabilities 回落唯一团队）。
    expect(screen.getByRole("button", { name: "Deploy" })).toBeEnabled();
    await user.click(screen.getByRole("button", { name: "Deploy" }));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "POST" && c.url.includes("/apps/demo/deployments"));
      expect(post).toBeTruthy();
      expect(post?.body).not.toHaveProperty("project");
    });
  });
});

// ── Deploy settings 卡（§4.7 新面：dokploy Deploy Settings 同构）──────────

/** Deploy settings stub：Me（角色）+ active revision + spec（无 cron）+
 * rollback POST 捕获 + Overview 其余查询兜底（GetApp 兜底形态可注入
 * suspended 位）。 */
function stubDeploySettings(
  role: string,
  isPlatformAdmin: boolean,
  opts: { revisions?: unknown; rollbackStatus?: number; appSuspended?: boolean } = {},
) {
  const calls: Array<{ url: string; method?: string; body?: unknown }> = [];
  const fetchMock = vi.fn().mockImplementation((url: string, init?: { method?: string; body?: string }) => {
    const u = String(url);
    calls.push({ url: u, method: init?.method, body: init?.body ? JSON.parse(init.body) : undefined });
    if (u.endsWith("/auth/me")) {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () =>
          Promise.resolve({
            user: { id: "01U1", email: "f@t.test", is_platform_admin: isPlatformAdmin },
            teams: [{ team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role }],
            project_overrides: [],
          }),
      });
    }
    if (u.endsWith("/spec")) {
      // 注意：spec 分支须先于 /revisions 的 includes 判断（spec URL 也含
      // /revisions 段——/apps/demo/revisions/{id}/spec）。
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () =>
          Promise.resolve({
            compose: JSON.stringify({ name: "demo", services: [{ name: "web", image: "nginx:1.27" }] }),
          }),
      });
    }
    if (u.includes("/apps/demo/revisions") && (!init?.method || init.method === "GET")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve(opts.revisions ?? { revisions: [{ id: "rev_act", status: "active" }] }) });
    }
    if (init?.method === "POST" && u.endsWith("/rollbacks")) {
      if (opts.rollbackStatus === 409) {
        return Promise.resolve({
          ok: false, status: 409, statusText: "",
          json: () => Promise.resolve({ code: "E_ROLLBACK_CONFLICT", message: "a deployment is already in flight", suggestion: "wait for it to finish" }),
        });
      }
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployment_id: "dep_re", desired_hash: "h" }) });
    }
    // GET /apps/demo（GetAppResponse）与其余查询兜底——挂起位可注入。
    return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve(opts.appSuspended ? { suspended: true } : {}) });
  });
  return { fetchMock, calls };
}

describe("AppOverviewPage deploy settings card (§4.7/§5)", () => {
  it("owner: Open terminal links to the terminal tab; the suspended note surfaces in-card", async () => {
    setToken("flt_test");
    const { fetchMock } = stubDeploySettings("owner", false);
    vi.stubGlobal("fetch", fetchMock);
    renderOverviewPage("/apps/demo", true);

    // Open terminal 直达 Terminal 页签（Redeploy/Stop/Start 动作自 §5 起在
    // 详情标题栏 AppHeaderActions——其行为测试在 components/
    // app-header-actions.test.tsx）。
    await waitFor(() =>
      expect(screen.getByTestId("deploy-settings")).toBeInTheDocument(),
    );
    expect(screen.getByTestId("open-terminal-link")).toHaveAttribute(
      "href",
      "/apps/demo/terminal",
    );
    // 非挂起态无挂起说明。
    expect(screen.queryByTestId("app-suspended-note")).not.toBeInTheDocument();
  });

  it("suspended app: in-card note explains the drain and points at the header Start action", async () => {
    setToken("flt_test");
    const { fetchMock } = stubDeploySettings("owner", false, { appSuspended: true });
    vi.stubGlobal("fetch", fetchMock);
    renderOverviewPage("/apps/demo", true);

    await waitFor(() =>
      expect(screen.getByTestId("app-suspended-note")).toHaveTextContent(
        "suspended",
      ),
    );
    expect(screen.getByTestId("app-suspended-note").textContent).toContain("Start");
  });

  it("platform admin: readonly note replaces the actions (P0-3), Deploy card absent", async () => {
    setToken("flt_test");
    const { fetchMock } = stubDeploySettings("owner", true);
    vi.stubGlobal("fetch", fetchMock);
    renderOverviewPage("/apps/demo", true);

    // Deploy settings 卡的说明 + 其余卡各自的说明（scaling 等）——断言
    // 「说明态存在且 Deploy 面全数退场」。
    await waitFor(() => {
      const notes = screen.getAllByTestId("platform-readonly-note");
      expect(notes.some((n) => n.textContent?.includes("Platform administrators have read-only access"))).toBe(true);
    });
    expect(screen.queryByTestId("deploy-card")).not.toBeInTheDocument();
  });

  it("viewer: neither the settings card nor the Deploy card renders (resource-write face)", async () => {
    setToken("flt_test");
    const { fetchMock } = stubDeploySettings("viewer", false);
    vi.stubGlobal("fetch", fetchMock);
    renderOverviewPage("/apps/demo", true);

    // Me 投影落地后的稳态：viewer 无部署能力面（fail-open 窗口先等关闭）。
    await waitFor(() =>
      expect(screen.queryByTestId("deploy-settings")).not.toBeInTheDocument(),
    );
    expect(screen.queryByTestId("deploy-card")).not.toBeInTheDocument();
  });
});
