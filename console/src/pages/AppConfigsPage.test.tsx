// app Configs 页测试（T 线 OT-3/IMPL-T1-4）：列表渲染（name/hash8/updated）、
// 新增提交（POST /configs 载荷）、编辑提交（同名覆盖即换版）、删除两步确认
// （对话框 → DELETE）、明文查看（GET，admin 门）、空态指引、viewer 只读
// （写面/明文查看门）。AppSecretsPage.test 同款 mock 形态。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AppConfigsPage } from "@/pages/AppConfigsPage";
import { TeamProjectProvider } from "@/lib/context";
import { setToken } from "@/api/client";

const CONFIGS = {
  configs: [
    { name: "app.yaml", hash8: "1a2b3c4d", updated_at: new Date().toISOString() },
    { name: "worker.yaml", hash8: "99887766", updated_at: new Date().toISOString() },
  ],
};

function stubConfigsFetch() {
  return vi.fn().mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    if (url.includes("/configs") && method === "POST") {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () => Promise.resolve({ app: "web", name: "app.yaml", hash8: "deadbeef" }),
      });
    }
    if (url.includes("/configs/app.yaml") && method === "DELETE") {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () => Promise.resolve({ app: "web", name: "app.yaml" }),
      });
    }
    if (url.includes("/configs/app.yaml")) {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () => Promise.resolve({ app: "web", name: "app.yaml", value: "listen: 8080\n", hash8: "1a2b3c4d" }),
      });
    }
    if (url.includes("/configs")) {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () => Promise.resolve(CONFIGS),
      });
    }
    return Promise.resolve({
      ok: true, status: 200, statusText: "",
      json: () => Promise.resolve({}),
    });
  });
}

function renderAt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={["/apps/web/configs"]}>
      <QueryClientProvider client={client}>
        <Routes>
          <Route path="/apps/:name/configs" element={<AppConfigsPage />} />
        </Routes>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

function renderAtInRole() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={["/apps/web/configs"]}>
      <QueryClientProvider client={client}>
        <TeamProjectProvider>
          <Routes>
            <Route path="/apps/:name/configs" element={<AppConfigsPage />} />
          </Routes>
        </TeamProjectProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("AppConfigsPage", () => {
  it("lists configs with content fingerprints and empty-state hint", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubConfigsFetch());

    renderAt();
    await screen.findByTestId("configs-page");

    const rows = await screen.findAllByTestId("config-row");
    expect(rows).toHaveLength(2);
    expect(screen.getByText("app.yaml")).toBeInTheDocument();
    expect(screen.getByText("1a2b3c4d")).toBeInTheDocument();
  });

  it("creates a config via the dialog (POST with app/name/value)", async () => {
    setToken("flt_test");
    const fetchMock = stubConfigsFetch();
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();

    renderAt();
    await screen.findByTestId("configs-page");

    await user.click(screen.getByTestId("config-new-button"));
    await user.type(screen.getByTestId("config-name-input"), "new.yaml");
    await user.type(screen.getByTestId("config-value-input"), "a: 1\n");
    await user.click(screen.getByTestId("config-save-submit"));

    await waitFor(() => {
      const posted = fetchMock.mock.calls.find(
        (c) => String(c[0]).includes("/configs") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(JSON.parse(String(posted![1]?.body))).toEqual({
        app: "web",
        name: "new.yaml",
        value: "a: 1\n",
      });
    });
  });

  it("edits an existing config through the same POST (overwrite = new version)", async () => {
    setToken("flt_test");
    const fetchMock = stubConfigsFetch();
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();

    renderAt();
    await screen.findAllByTestId("config-row");
    await user.click(screen.getAllByTestId("config-edit-button")[0]);

    // 名称锁定为当前行（防误改名——改名 = 新增 + 旧名悬空）。
    expect(screen.getByTestId("config-name-input")).toBeDisabled();
    await user.type(screen.getByTestId("config-value-input"), "listen: 9090\n");
    await user.click(screen.getByTestId("config-save-submit"));

    await waitFor(() => {
      const posted = fetchMock.mock.calls.find(
        (c) => String(c[0]).includes("/configs") && c[1]?.method === "POST",
      );
      expect(posted).toBeTruthy();
      expect(JSON.parse(String(posted![1]?.body))).toEqual({
        app: "web",
        name: "app.yaml",
        value: "listen: 9090\n",
      });
    });
  });

  it("removes a config only through the confirm dialog (DELETE)", async () => {
    setToken("flt_test");
    const fetchMock = stubConfigsFetch();
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();

    renderAt();
    await screen.findAllByTestId("config-row");

    await user.click(screen.getAllByTestId("config-remove-button")[0]);
    expect(screen.getByTestId("config-remove-dialog")).toHaveTextContent(/E_CONFIG_NOT_FOUND/);
    await user.click(screen.getByTestId("config-remove-submit"));

    await waitFor(() => {
      const deleted = fetchMock.mock.calls.find(
        (c) => String(c[0]).includes("/configs/app.yaml") && c[1]?.method === "DELETE",
      );
      expect(deleted).toBeTruthy();
    });
  });

  it("reads the plaintext content back through the admin-gated view dialog", async () => {
    setToken("flt_test");
    const fetchMock = stubConfigsFetch();
    vi.stubGlobal("fetch", fetchMock);
    const user = userEvent.setup();

    renderAt();
    await screen.findAllByTestId("config-row");

    await user.click(screen.getAllByTestId("config-view-button")[0]);
    await waitFor(() =>
      expect(screen.getByTestId("config-view-content")).toHaveTextContent("listen: 8080"),
    );
    // Get 走明文回读端点（admin 门在服务端）。
    expect(
      fetchMock.mock.calls.some(
        (c) => String(c[0]).endsWith("/configs/app.yaml") && (c[1]?.method ?? "GET") === "GET",
      ),
    ).toBe(true);
  });

  it("renders the empty-state hint when no configs are stored", async () => {
    setToken("flt_test");
    vi.stubGlobal(
      "fetch",
      vi.fn().mockImplementation(() =>
        Promise.resolve({
          ok: true,
          status: 200,
          statusText: "",
          json: () => Promise.resolve({ configs: [] }),
        }),
      ),
    );

    renderAt();
    await screen.findByTestId("configs-empty");
  });

  it("viewer: no write controls, no plaintext view, read-only empty state", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubConfigsFetchRole({ role: "viewer", configs: [] }));

    renderAtInRole();
    const empty = await screen.findByTestId("configs-empty");
    await waitFor(() =>
      expect(empty.textContent).toContain("Declare it under the compose file's top-level"),
    );
    await waitFor(() =>
      expect(screen.queryByTestId("config-new-button")).not.toBeInTheDocument(),
    );
    expect(screen.queryByTestId("config-view-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("config-remove-button")).not.toBeInTheDocument();
    expect(screen.queryByTestId("platform-readonly-note")).not.toBeInTheDocument();
  });
});

/** 带角色视角的 fetch 桩（/auth/me + configs 投影）。 */
function stubConfigsFetchRole(opts: { role: string; configs: unknown[] }) {
  return vi.fn().mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.endsWith("/auth/me")) {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () =>
          Promise.resolve({
            user: { id: "01U1", email: "f@t.test", is_platform_admin: false },
            teams: [
              { team_id: "01TEAM", team_slug: "acme", team_name: "Acme", role: opts.role },
            ],
            project_overrides: [],
          }),
      });
    }
    if (url.includes("/configs")) {
      return Promise.resolve({
        ok: true, status: 200, statusText: "",
        json: () => Promise.resolve({ configs: opts.configs }),
      });
    }
    return Promise.resolve({
      ok: true, status: 200, statusText: "",
      json: () => Promise.resolve({}),
    });
  });
}
