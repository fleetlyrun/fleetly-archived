// 部署历史页测试（2026-09-29 §4.7 三轮重设计后本页 = 纯历史面：Deploy 卡
// 上移 Overview，此处不再渲染）。覆盖：
//   - 失败行错误信封形态（error_code + verdict + recovery 可见）；中间态
//     徽章（observing/blocked_waiting）一等渲染；跟踪轮询持续失败口径
//     （M9-5）已随 Deploy 卡迁至 AppOverviewPage.deploy.test.tsx。
//   - 时长列：终态行 = created_at → updated_at（入队→终态，updated_at 是
//     state 层自证的终态写入时刻近似）；进行中行 = 已流逝时间；缺时间戳
//     如实「—」。
//   - 批量取消（Cancel queued）：非终态行计数入钮、确认框列明、逐条调用
//     取消；部分 409（已切流/终态）如实汇报成败计数（服务端取消语义受限：
//     未切流才可取消）。
//   - 重部署 webhook URL 行（admin+）：业务名寻址；developer 无此行且不发
//     webhook 读请求（注定 403）。
//   - 平台管理员双门（P0-3）：只读说明卡原位；行内写操作按能力门不渲染。
//   - 行内回滚：确认框 + POST /rollbacks 载荷断言（该行 revision）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { AppDeploymentsPage } from "@/pages/AppDeploymentsPage";
import { TeamProjectProvider } from "@/lib/context";
import { setToken } from "@/api/client";

function ok(body: unknown) {
  return {
    ok: true,
    status: 200,
    statusText: "",
    json: () => Promise.resolve(body),
  };
}

function stubFetch(deployments: unknown[]) {
  return vi.fn().mockImplementation((url: string) => {
    if (String(url).includes("/deployments")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ deployments }),
      });
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      statusText: "",
      json: () => Promise.resolve({ revisions: [] }),
    });
  });
}

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={["/apps/demo/deployments"]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route path="/apps/:name/deployments" element={<AppDeploymentsPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("AppDeploymentsPage failure envelope", () => {
  it("renders code + message + suggestion for a failed deployment row", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      stubFetch([
        {
          id: "dep_01",
          app: "demo",
          kind: "deploy",
          status: "failed",
          phase: "",
          revision_id: "rev_1",
          error_code: "E_BUILD_FAILED",
          verdict: "buildkit solve failed at step 4/6",
          recovery: "fix Dockerfile and redeploy; build log in the artifacts dir",
          created_at: "2026-09-18T10:00:00Z",
        },
      ]),
    );

    renderPage();

    await waitFor(() => screen.getByRole("alert"));
    const alert = screen.getByRole("alert");
    expect(alert).toHaveTextContent("E_BUILD_FAILED");
    expect(alert).toHaveTextContent("buildkit solve failed at step 4/6");
    expect(alert).toHaveTextContent(
      "fix Dockerfile and redeploy; build log in the artifacts dir",
    );
  });

  it("renders intermediate states (observing / blocked_waiting) as first-class badges", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      stubFetch([
        {
          id: "dep_obs",
          app: "demo",
          kind: "deploy",
          status: "observing",
          phase: "",
          revision_id: "rev_2",
          error_code: "",
          verdict: "",
          recovery: "",
        },
        {
          id: "dep_bw",
          app: "demo",
          kind: "deploy",
          status: "releasing",
          phase: "blocked_waiting",
          revision_id: "rev_3",
          error_code: "",
          verdict: "",
          recovery: "",
        },
      ]),
    );

    renderPage();

    await waitFor(() => screen.getByText("dep_bw"));
    const rows = screen.getAllByTestId("deployment-row");
    expect(rows).toHaveLength(2);
    const badges = screen.getAllByTestId("state-badge");
    const states = badges.map((b) => b.getAttribute("data-state"));
    expect(states).toContain("observing");
    expect(states).toContain("blocked_waiting");
  });
});

// ── 时长列（dokploy 每行耗时徽章同构）────────────────────────────────────

describe("AppDeploymentsPage duration column", () => {
  it("terminal rows show enqueue→terminal duration; in-flight rows show elapsed; missing timestamps render —", async () => {
    setToken("flt_test");
    const now = Date.now();
    vi.stubGlobal(
      "fetch",
      stubFetch([
        {
          id: "dep_done",
          app: "demo",
          kind: "deploy",
          status: "succeeded",
          phase: "",
          revision_id: "rev_1",
          created_at: new Date(now - 45_000).toISOString(),
          updated_at: new Date(now - 0).toISOString(),
        },
        {
          id: "dep_run",
          app: "demo",
          kind: "deploy",
          status: "building",
          phase: "",
          created_at: new Date(now - 2 * 3_600_000).toISOString(),
        },
        {
          id: "dep_old",
          app: "demo",
          kind: "deploy",
          status: "failed",
          phase: "",
          created_at: "2026-09-18T10:00:00Z",
        },
      ]),
    );

    renderPage();

    const rows = await screen.findAllByTestId("deployment-row");
    expect(rows).toHaveLength(3);
    const cells = screen.getAllByTestId("deployment-duration");
    expect(cells).toHaveLength(3);
    // 终态行：45s（created→updated 固定差）。
    expect(cells[0]?.textContent).toBe("45s");
    // 进行中行：已流逝 2h 量级（不精确到秒——轮询节拍间会漂）。
    expect(cells[1]?.textContent).toMatch(/^2h \d+m$/);
    // 缺 updated_at 的终态行：无从计算 → 如实「—」（不冒充 0s）。
    expect(cells[2]?.textContent).toBe("—");
  });
});

// ── 批量取消（dokploy Cancel Queues 同构）────────────────────────────────

describe("AppDeploymentsPage cancel queued", () => {
  const ROWS = [
    {
      id: "dep_q1",
      app: "demo",
      kind: "deploy",
      status: "queued",
      phase: "",
      created_at: "2026-09-29T10:00:00Z",
    },
    {
      id: "dep_q2",
      app: "demo",
      kind: "deploy",
      status: "releasing",
      phase: "blocked_waiting",
      created_at: "2026-09-29T10:01:00Z",
    },
    {
      id: "dep_ok",
      app: "demo",
      kind: "deploy",
      status: "succeeded",
      phase: "",
      revision_id: "rev_1",
      created_at: "2026-09-28T10:00:00Z",
    },
  ];

  function stubCancel(ops: { failIds?: string[] } = {}) {
    const calls: Array<{ url: string; method?: string }> = [];
    const fetchMock = vi.fn().mockImplementation((url: string, init?: { method?: string }) => {
      const u = String(url);
      calls.push({ url: u, method: init?.method });
      if (init?.method === "POST" && u.endsWith("/cancel")) {
        const id = u.includes("dep_q1") ? "dep_q1" : u.includes("dep_q2") ? "dep_q2" : "";
        if (ops.failIds?.includes(id ?? "")) {
          return Promise.resolve({
            ok: false, status: 409, statusText: "",
            json: () => Promise.resolve({ code: "E_DEPLOYMENT_NOT_CANCELLABLE", message: "already switched traffic", suggestion: "wait for terminal state" }),
          });
        }
        return Promise.resolve(ok({ id, status: "cancelled" }));
      }
      if (u.includes("/deployments")) {
        return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployments: ROWS }) });
      }
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ revisions: [] }) });
    });
    return { fetchMock, calls };
  }

  it("cancels every in-flight deployment after confirm; terminal rows are not touched", async () => {
    setToken("flt_test");
    const { fetchMock, calls } = stubCancel();
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    const rows = await screen.findAllByTestId("deployment-row");
    expect(rows).toHaveLength(3);
    // 钮上计数只含非终态行（succeeded 不计）。
    const open = screen.getByTestId("deployments-cancel-queued");
    expect(open).toHaveTextContent("Cancel queued (2)");
    fireEvent.click(open);

    const dialog = await screen.findByTestId("deployments-cancel-queued-dialog");
    expect(dialog.textContent).toContain("dep_q1");
    expect(dialog.textContent).toContain("dep_q2");
    fireEvent.click(screen.getByTestId("deployments-cancel-queued-confirm"));

    await waitFor(() => {
      const cancels = calls.filter((c) => c.method === "POST" && c.url.endsWith("/cancel"));
      expect(cancels).toHaveLength(2);
      expect(calls.some((c) => c.url.includes("dep_ok") && c.url.endsWith("/cancel"))).toBe(false);
    });
    // 全部成功：对话框关闭。
    await waitFor(() =>
      expect(screen.queryByTestId("deployments-cancel-queued-dialog")).not.toBeInTheDocument(),
    );
  });

  it("reports partial refusal honestly when the server refuses already-switched deployments", async () => {
    setToken("flt_test");
    const { fetchMock } = stubCancel({ failIds: ["dep_q2"] });
    vi.stubGlobal("fetch", fetchMock);
    renderPage();

    fireEvent.click(await screen.findByTestId("deployments-cancel-queued"));
    fireEvent.click(screen.getByTestId("deployments-cancel-queued-confirm"));

    await waitFor(() =>
      expect(screen.getByTestId("deployments-cancel-queued-result")).toHaveTextContent(
        "refused cancellation",
      ),
    );
    // 部分失败：对话框保持打开（结果如实呈现，不静默关闭制造全成假象）。
    expect(screen.getByTestId("deployments-cancel-queued-dialog")).toBeInTheDocument();
  });
});

// ── 重部署 webhook URL 行（admin+）+ 平台管理员双门（P0-3）────────────────

function stubMe(role: string, isPlatformAdmin: boolean, deployments: unknown[]) {
  const calls: Array<{ url: string; method?: string }> = [];
  const fetchMock = vi.fn().mockImplementation((url: string, init?: { method?: string }) => {
    const u = String(url);
    calls.push({ url: u, method: init?.method });
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
    if (u.endsWith("/webhook") && (!init?.method || init.method === "GET")) {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () => Promise.resolve({ name: "demo", secret_configured: true, source_branch: "main" }),
      });
    }
    if (u.includes("/deployments")) {
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployments }) });
    }
    return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ revisions: [] }) });
  });
  return { fetchMock, calls };
}

function renderPageInTeamContext() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={["/apps/demo/deployments"]}>
      <QueryClientProvider client={client}>
        <TeamProjectProvider>
          <Routes>
            <Route path="/apps/:name/deployments" element={<AppDeploymentsPage />} />
          </Routes>
        </TeamProjectProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

describe("AppDeploymentsPage platform-admin read-only + webhook row", () => {
  it("平台管理员：只读说明卡原位，无行内写钮，webhook 行不渲染（能力门关门不发读请求）", async () => {
    setToken("flt_test");
    const { fetchMock, calls } = stubMe("owner", true, []);
    vi.stubGlobal("fetch", fetchMock);
    renderPageInTeamContext();

    // 说明卡在原位（诚实说明，不做静默消失）。
    await waitFor(() => {
      expect(screen.getByTestId("platform-readonly-note")).toHaveTextContent(
        "Platform administrators have read-only access to resources",
      );
    });
    // 页面骨架不塌：部署历史空态照常。
    await waitFor(() => expect(screen.getByText("No deployments yet.")).toBeInTheDocument());
    // 资源面写卡/写钮不再渲染（Deploy 卡已上移 Overview；回滚/取消按能力门）。
    expect(screen.queryByTestId("deploy-card")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Rollback" })).not.toBeInTheDocument();
    // 平台管理员能力门统一关门 → webhook 读面不发（注定 403 的请求不发）。
    expect(screen.queryByTestId("history-webhook-row")).not.toBeInTheDocument();
    expect(calls.some((c) => c.url.endsWith("/webhook"))).toBe(false);
  });

  it("非管理员 owner：重部署 webhook URL 行渲染（业务名寻址），无只读说明（防回归）", async () => {
    setToken("flt_test");
    const { fetchMock } = stubMe("owner", false, []);
    vi.stubGlobal("fetch", fetchMock);
    renderPageInTeamContext();

    await waitFor(() =>
      expect(screen.getByTestId("history-webhook-url")).toHaveTextContent(
        "/v1/apps/demo/webhooks/github",
      ),
    );
    expect(screen.queryByTestId("platform-readonly-note")).not.toBeInTheDocument();
    // Deploy 卡已上移 Overview——本页不再渲染（结构性断言，防回移）。
    expect(screen.queryByTestId("deploy-card")).not.toBeInTheDocument();
  });

  it("developer：webhook 行不渲染且不发 webhook 读请求（admin scope 注定 403）", async () => {
    setToken("flt_test");
    const { fetchMock, calls } = stubMe("developer", false, []);
    vi.stubGlobal("fetch", fetchMock);
    renderPageInTeamContext();

    // Me 投影落地（fail-open 窗口关闭）后再断言行为稳态。
    await waitFor(() => expect(screen.getByText("No deployments yet.")).toBeInTheDocument());
    await waitFor(() => {
      expect(screen.queryByTestId("history-webhook-row")).not.toBeInTheDocument();
      expect(calls.some((c) => c.url.endsWith("/webhook"))).toBe(false);
    });
  });
});

// ── 行内回滚（2026-09-29 二次重设计：替代独立 Rollback 卡）────────────────
// 历史里选要回去的那一行 → 确认框承载语义 → POST /rollbacks 载荷携带该行
// revision；无 revision 的行（失败/进行中）不出回滚钮。
describe("AppDeploymentsPage per-row rollback", () => {
  const ROWS = [
    {
      id: "dep_new",
      app: "demo",
      kind: "deploy",
      status: "succeeded",
      phase: "",
      revision_id: "rev_2",
      created_at: "2026-09-29T10:00:00Z",
      updated_at: "2026-09-29T10:00:20Z",
    },
    {
      id: "dep_old",
      app: "demo",
      kind: "deploy",
      status: "succeeded",
      phase: "",
      revision_id: "rev_1",
      created_at: "2026-09-28T10:00:00Z",
      updated_at: "2026-09-28T10:00:21Z",
    },
  ];

  function stubRollback(calls: Array<{ url: string; method?: string; body?: unknown }>) {
    return vi.fn().mockImplementation((url: string, init?: { method?: string; body?: string }) => {
      const u = String(url);
      calls.push({ url: u, method: init?.method, body: init?.body ? JSON.parse(init.body) : undefined });
      if (init?.method === "POST" && u.endsWith("/rollbacks")) {
        return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployment_id: "dep_rb", desired_hash: "h" }) });
      }
      if (u.includes("/deployments")) {
        return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ deployments: ROWS }) });
      }
      return Promise.resolve({ ok: true, status: 200, statusText: "", json: () => Promise.resolve({ revisions: [] }) });
    });
  }

  it("rolls back to the clicked row's revision after confirm", async () => {
    setToken("flt_test");
    const calls: Array<{ url: string; method?: string; body?: unknown }> = [];
    vi.stubGlobal("fetch", stubRollback(calls));

    renderPage();
    const rows = await screen.findAllByTestId("deployment-row");
    expect(rows).toHaveLength(2);

    // 每个带 revision 的行有 Rollback 钮；点旧行（dep_old）。
    const rollbackButtons = screen.getAllByTestId("deployment-rollback");
    expect(rollbackButtons).toHaveLength(2);
    fireEvent.click(rollbackButtons[1]!);

    const dialog = await screen.findByTestId("deployment-rollback-dialog");
    expect(dialog).toHaveTextContent("Roll back to this deployment?");
    fireEvent.click(screen.getByTestId("deployment-rollback-confirm"));

    await waitFor(() => {
      const post = calls.find((c) => c.method === "POST" && c.url.endsWith("/rollbacks"));
      expect(post?.url.endsWith("/v1/apps/demo/rollbacks")).toBe(true);
      // 载荷携带被点行的 revision（回滚目标 = 该行固化的版本）。
      expect(post?.body).toEqual({ target_revision_id: "rev_1" });
    });
    expect(screen.queryByTestId("deployment-rollback-dialog")).not.toBeInTheDocument();
  });
});
