// 系统页 join 向导卡片测试（E1-8，multi-node §2.3/§2.9）：生成向导材料
//（join 命令/token 复制面）、HA 边界诚实口径卡片、既有节点表锚点不破坏。
// 写面门（2026-09-25 走查）：join 面整体 admin scope——非平台管理员卡片
// 整体替换为只读说明，viewer 不再看到 enabled 的 Generate/Rotate 假按钮。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router-dom";
import { describe, expect, it, vi } from "vitest";

import { SystemPage } from "@/pages/SystemPage";
import { setToken } from "@/api/client";

function stubSystemFetch(
  joinGuide?: Record<string, unknown>,
  statusExtra: Record<string, unknown> = {},
  isPlatformAdmin = true,
) {
  return vi.fn().mockImplementation((input: RequestInfo | URL) => {
    const url = String(input);
    if (url.endsWith("/auth/me")) {
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
    if (url.includes("/system/nodes/join-guide")) {
      if (!joinGuide) {
        return Promise.resolve({
          ok: false,
          status: 409,
          statusText: "Conflict",
          json: () =>
            Promise.resolve({
              code: "E_MULTI_NODE_REQUIRES_BASE_DOMAIN",
              message: "multi-node is not enabled: base_domain is not configured",
              suggestion: "Configure base_domain before joining nodes.",
            }),
        });
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ guide: joinGuide }),
      });
    }
    if (url.includes("/system/nodes")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () =>
          Promise.resolve({
            nodes: [
              {
                swarm_node_id: "sw-1",
                hostname: "srv-01",
                state: "ready",
                availability: "active",
                is_manager: true,
                platform_id: "n_node1",
                pinned_app_ids: ["app-1"],
              },
            ],
          }),
      });
    }
    if (url.includes("/system/ingress")) {
      return Promise.resolve({
        ok: true,
        status: 200,
        statusText: "",
        json: () => Promise.resolve({ certificates: [] }),
      });
    }
    return Promise.resolve({
      ok: true,
      status: 200,
      statusText: "",
      json: () =>
        Promise.resolve({
          version: "dev",
          components: [{ name: "state.store", ok: true }],
          ...statusExtra,
        }),
    });
  });
}

function renderSystem() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <MemoryRouter initialEntries={["/system?tab=nodes"]}>
      <QueryClientProvider client={client}>
        <SystemPage />
      </QueryClientProvider>
    </MemoryRouter>,
  );
}

const GUIDE = {
  join_command: "docker swarm join --token swmtkn-x 198.51.100.10:2377",
  manager_addr: "198.51.100.10",
  worker_token: "swmtkn-x",
  base_domain: "example.test",
  manager_firewall_rules: [
    {
      direction: "worker_to_manager",
      port: "2377/tcp",
      purpose: "cluster management",
      side: "manager",
      rule: "iptables -A INPUT -p tcp -s 203.0.113.9 --dport 2377 -j ACCEPT",
    },
  ],
  worker_firewall_rules: [],
  worker_preflight_commands: ["docker version"],
  dns_steps: ["ctrl.example.test keeps pointing at the manager only"],
  completion_checks: ["node.joined event with a non-empty platform_id"],
};

describe("SystemPage join wizard (E1-8)", () => {
  it("generates the join guide with command, token and rules", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubSystemFetch(GUIDE));
    const user = userEvent.setup();

    renderSystem();
    await waitFor(() => screen.getByText("srv-01"));
    await user.click(screen.getByText("Generate join guide"));

    await waitFor(() =>
      expect(screen.getByTestId("join-wizard-command").textContent).toContain(
        "docker swarm join --token swmtkn-x 198.51.100.10:2377",
      ),
    );
    expect(screen.getByTestId("join-wizard-token").textContent).toContain("swmtkn-x");
    expect(screen.getByText("2377/tcp")).toBeInTheDocument();
    expect(screen.getByText(/manager only/)).toBeInTheDocument();
    expect(screen.getByText(/node.joined/)).toBeInTheDocument();
  });

  it("renders the D-MN-13 gate envelope when base_domain is missing", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubSystemFetch(undefined));
    const user = userEvent.setup();

    renderSystem();
    // Me 投影解析后写面 UI 才渲染——先等 Generate 按钮出现。
    await user.click(await screen.findByText("Generate join guide"));

    await waitFor(() =>
      expect(
        screen.getByText(/E_MULTI_NODE_REQUIRES_BASE_DOMAIN/),
      ).toBeInTheDocument(),
    );
  });

  it("viewer（写面门，2026-09-25 走查）：向导整体替换为只读说明，无 Generate/Rotate 假按钮", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubSystemFetch(GUIDE, {}, false));

    renderSystem();
    await waitFor(() => screen.getByText("srv-01"));

    expect(screen.getByTestId("join-wizard-readonly-note")).toHaveTextContent(
      "Platform administrator required.",
    );
    expect(screen.queryByText("Generate join guide")).not.toBeInTheDocument();
    expect(screen.queryByTestId("join-token-rotate")).not.toBeInTheDocument();
    expect(screen.queryByTestId("join-wizard-command")).not.toBeInTheDocument();
  });

  it("shows the HA boundary card with get / do-not-get columns", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubSystemFetch(GUIDE));

    renderSystem();
    const ha = await screen.findByTestId("ha-boundary");
    expect(ha.textContent).toContain("You get");
    expect(ha.textContent).toContain("You do not get");
    expect(ha.textContent).toContain("Management-plane HA");
    expect(ha.textContent).toContain("Stateless process-level HA");
  });

  it("keeps the nodes table anchors intact", async () => {
    setToken("flt_test");
    vi.stubGlobal("fetch", stubSystemFetch(GUIDE));

    renderSystem();
    await waitFor(() => screen.getByText("srv-01"));
    // 回到 components 页签：既有锚点 component-health 卡片仍在。
    await userEvent.click(screen.getByRole("tab", { name: "Components" }));
    await waitFor(() =>
      expect(screen.getByTestId("component-health")).toBeInTheDocument(),
    );
    // 回到 nodes 页签：平台 ID 列照常渲染。
    await userEvent.click(screen.getByRole("tab", { name: "Nodes" }));
    expect(screen.getByText("n_node1")).toBeInTheDocument();
  });
});
