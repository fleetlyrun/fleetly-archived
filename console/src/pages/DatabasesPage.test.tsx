// 库实例列表测试（E4 W4-S6）：一等资源面渲染（状态徽章/升级位）、创建
// 对话框的形状校验与提交路径（mock fetch 断言 POST /v1/databases 载荷）、
// 空态。data-testid 锚点为冻结契约（只增）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { DatabasesPage } from "@/pages/DatabasesPage";
import { TeamProjectProvider } from "@/lib/context";
import { setToken } from "@/api/client";

const DBS = [
  {
    id: "db1",
    name: "pg-prod",
    template: "postgres-16",
    status: "ready",
    placement: "node-a",
    volume: { name: "fleetly-db-pg-prod-data-ab12cd34", status: "active" },
    limits: { memory_bytes: "1073741824" },
  },
  {
    id: "db2",
    name: "cache",
    template: "redis-7",
    status: "degraded",
    upgrade_available: true,
  },
];

function stubFetchWith(dbs: unknown[]) {
  return vi.fn().mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes("/databases")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ databases: dbs }),
      });
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      statusText: "",
      json: () => Promise.resolve({ backups: [] }),
    });
  });
}

function renderAt(route: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={[route]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route path="/databases" element={<DatabasesPage />} />
          <Route path="/databases/:name" element={<p>detail-of-db</p>} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("DatabasesPage list", () => {
  it("renders one row per instance with state badge and upgrade marker", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubFetchWith(DBS));

    renderAt("/databases");

    await waitFor(() => {
      expect(screen.getAllByTestId("database-row")).toHaveLength(2);
    });
    expect(screen.getByText("pg-prod")).toBeInTheDocument();
    expect(screen.getByText("cache")).toBeInTheDocument();
    const badges = screen.getAllByTestId("state-badge");
    expect(badges).toHaveLength(2);
    expect(screen.getByTestId("database-upgrade-available")).toBeInTheDocument();
  });

  it("shows the empty state when no instances exist", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubFetchWith([]));

    renderAt("/databases");

    await screen.findByText("No database instances yet.");
    expect(screen.getByTestId("databases-page")).toBeInTheDocument();
  });

  it("navigates to the detail on row click", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubFetchWith(DBS));
    const user = userEvent.setup();

    renderAt("/databases");
    await screen.findByText("pg-prod");
    await user.click(screen.getByText("pg-prod"));

    expect(await screen.findByText("detail-of-db")).toBeInTheDocument();
  });
});

describe("DatabasesPage create dialog", () => {
  it("rejects an invalid name and posts a valid create payload", async () => {
    setToken("flt_test");
    const fetchMock = vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (url.includes("/databases") && (!init || init.method === "GET" || init.method === undefined)) {
        return Promise.resolve({
          ok: true,
          status: 200,
          statusText: "",
          json: () => Promise.resolve({ databases: DBS }),
        });
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ databases: DBS, database: { id: "dbx" } }),
      });
    });
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();

    renderAt("/databases");
    await screen.findByText("pg-prod");

    await user.click(screen.getByTestId("database-create-button"));
    const dialog = screen.getByTestId("database-create-dialog");
    expect(dialog).toBeInTheDocument();

    // 大写与空格违反名字规则 → 提交按钮不可点。
    const nameInput = screen.getByTestId("database-name-input");
    await user.type(nameInput, "Bad Name");
    expect(screen.getByTestId("database-create-submit")).toBeDisabled();

    // 合法名 + 可选限额 → POST 载荷携带 template 与换算后的限额。
    await user.clear(nameInput);
    await user.type(nameInput, "pg-new");
    await user.type(screen.getByTestId("database-cpu-input"), "2");
    await user.type(screen.getByTestId("database-memory-input"), "1");
    const submit = screen.getByTestId("database-create-submit");
    expect(submit).toBeEnabled();
    await user.click(submit);

    await waitFor(() => {
      const posted = fetchMock.mock.calls.find(
        (c) => String(c[0]).endsWith("/databases") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      const body = JSON.parse(String(posted![1]?.body));
      expect(body.name).toBe("pg-new");
      expect(body.template).toBe("postgres-16");
      expect(body.limits.cpu_seconds).toBe(2);
      expect(body.limits.memory_bytes).toBe(String(1024 * 1024 * 1024));
    });
    // 成功后对话框关闭。
    await waitFor(() => {
      expect(screen.queryByTestId("database-create-dialog")).not.toBeInTheDocument();
    });
  });

  it("offers the PG 18 templates (incl. the percona/pgvector distribution) and posts the percona selection", async () => {
    setToken("flt_test");
    const posted: Array<Record<string, unknown>> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const method = init?.method ?? "GET";
        if (url.includes("/databases") && method === "POST") {
          posted.push(JSON.parse(String(init?.body ?? "{}")));
          return Promise.resolve({
            ok: true,
            status: 200,
            statusText: "",
            json: () => Promise.resolve({ database: { id: "dbx", name: "pg-vec" } }),
          });
        }
        if (url.includes("/databases")) {
          return Promise.resolve({
            ok: true,
            status: 200,
            statusText: "",
            json: () => Promise.resolve({ databases: DBS }),
          });
        }
        return Promise.resolve({
          ok: true,
          status: 200,
          statusText: "",
          json: () => Promise.resolve({ backups: [] }),
        });
      }),
    );
    const user = userEvent.setup();

    renderAt("/databases");
    await screen.findByText("pg-prod");

    await user.click(screen.getByTestId("database-create-button"));
    // 模板选择器列出两新条目（与 server 词表一致）；radix Select 在 jsdom
    // 下用 fireEvent 驱动（交互纪律同 s3-settings-card 用例）。
    fireEvent.keyDown(screen.getByTestId("database-template-select"), { key: "ArrowDown" });
    expect(await screen.findByRole("option", { name: "postgres-18" })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("option", { name: "percona-postgresql-18" }));

    await user.type(screen.getByTestId("database-name-input"), "pg-vec");
    await user.click(screen.getByTestId("database-create-submit"));
    await waitFor(() => {
      expect(posted).toHaveLength(1);
      expect(posted[0].name).toBe("pg-vec");
      expect(posted[0].template).toBe("percona-postgresql-18");
    });
  });

  it("displays the target project in the dialog (no context selection = server default)", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubFetchWith(DBS));
    const user = userEvent.setup();

    renderAt("/databases");
    await screen.findByText("pg-prod");

    await user.click(screen.getByTestId("database-create-button"));
    // 抽出的对话框显式展示目标项目（可发现性收口）；本测试无顶栏项目选择
    // ——回落服务端缺省（调用者个人队 default 项目）。
    expect(screen.getByTestId("database-target-project")).toHaveTextContent(
      "default (your personal team)",
    );
  });

  it("follows the topbar team/project context: dialog shows team/prj and the payload carries it", async () => {
    // 顶栏上下文跟随（2026-09-25 走查：建库对话框恒 default personal team
    // 的实录缺陷——projectRef 必须响应式取自 useProjectContext 的选中
    // slugs 组合）。本地存储播种选中态（TeamProjectProvider 持久化键）。
    setToken("flt_test");
    window.localStorage.setItem(
      "fleetly.console.context",
      JSON.stringify({ team: "review", project: "review" }),
    );
    const posted: Array<{ url: string; body: Record<string, unknown> }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input);
        const method = init?.method ?? "GET";
        if (url.endsWith("/auth/me")) {
          return Promise.resolve({
            ok: true, status: 200, statusText: "",
            json: () =>
              Promise.resolve({
                user: { id: "01U1", email: "f@t.test", is_platform_admin: false },
                teams: [
                  { team_id: "01T", team_slug: "review", team_name: "Review", role: "owner" },
                ],
                project_overrides: [],
              }),
          });
        }
        if (url.endsWith("/projects")) {
          return Promise.resolve({
            ok: true, status: 200, statusText: "",
            json: () =>
              Promise.resolve({
                projects: [
                  { id: "01P", team_id: "01T", team_slug: "review", slug: "review", name: "Review" },
                ],
              }),
          });
        }
        if (url.includes("/databases") && method === "POST") {
          posted.push({ url, body: JSON.parse(String(init?.body ?? "{}")) });
          return Promise.resolve({
            ok: true, status: 200, statusText: "",
            json: () => Promise.resolve({ database: { id: "dbx", name: "pg-new" } }),
          });
        }
        if (url.includes("/databases")) {
          return Promise.resolve({
            ok: true, status: 200, statusText: "",
            json: () => Promise.resolve({ databases: DBS }),
          });
        }
        return Promise.resolve({
          ok: true, status: 200, statusText: "",
          json: () => Promise.resolve({ backups: [] }),
        });
      }),
    );
    const user = userEvent.setup();

    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <MemoryRouter initialEntries={["/databases"]}>
        <QueryClientProvider client={client}>
          <TeamProjectProvider>
            <Routes>
              <Route path="/databases" element={<DatabasesPage />} />
              <Route path="/databases/:name" element={<p>detail-of-db</p>} />
            </Routes>
          </TeamProjectProvider>
        </QueryClientProvider>
      </MemoryRouter>,
    );
    await screen.findByText("pg-prod");

    await user.click(screen.getByTestId("database-create-button"));
    // 对话框内显式展示选中上下文（review/review）。
    expect(screen.getByTestId("database-target-project")).toHaveTextContent("review/review");

    await user.type(screen.getByTestId("database-name-input"), "pg-new");
    await user.click(screen.getByTestId("database-create-submit"));
    await waitFor(() => {
      expect(posted).toHaveLength(1);
      // 创建载荷携带限定形目标项目（与列表收窄同源）。
      expect(posted[0].body.project).toBe("review/review");
      expect(posted[0].body.name).toBe("pg-new");
    });
  });
});
