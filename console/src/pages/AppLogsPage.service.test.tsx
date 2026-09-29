// 日志页 Service 下拉的钉测（独立成文件的原因：radix Select 的打开有
// 模块级状态残留——同文件里任何先开过 Select 的用例都会让后续用例的
// 指针打开失效，见 AppLogsPage.test.tsx 文件头注；vitest 文件间模块
// 隔离，单用例单文件即天然免疫）。
//
// 背景：下拉服务名取自 GetRevisionSpec 的归一化快照，其形态同构
// compose.Spec——services 是数组、name 在元素上（并非 compose 原文的
// map 形态）。修复前本地解析按 map 取 Object.keys，数组下标「0」「1」
// 冒充服务名（2026-09-29 用户报告）。

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { AppLogsPage } from "@/pages/AppLogsPage";

// jsdom 缺 radix Select 依赖的 pointer capture API（仅本文件注入）。
if (!Element.prototype.hasPointerCapture) {
  Element.prototype.hasPointerCapture = () => false;
  Element.prototype.releasePointerCapture = () => undefined;
  Element.prototype.setPointerCapture = () => undefined;
}

const apiMocks = vi.hoisted(() => ({
  listRevisions: vi.fn(),
  getRevisionSpec: vi.fn(),
  listHistoryLogs: vi.fn(),
  followLogs: vi.fn(),
}));

vi.mock("@/api/endpoints", () => ({
  listRevisions: apiMocks.listRevisions,
  getRevisionSpec: apiMocks.getRevisionSpec,
  listHistoryLogs: apiMocks.listHistoryLogs,
}));

vi.mock("@/api/streams", () => {
  class StreamError extends Error {
    status: number;
    constructor(status: number, _details: unknown) {
      super(`stream error ${status}`);
      this.status = status;
    }
  }
  return { StreamError, followLogs: apiMocks.followLogs };
});

beforeEach(() => {
  vi.clearAllMocks();
  apiMocks.listRevisions.mockResolvedValue({ revisions: [] });
  apiMocks.listHistoryLogs.mockResolvedValue({ entries: [] });
  apiMocks.followLogs.mockResolvedValue({ close: vi.fn() });
});

describe("AppLogsPage service dropdown", () => {
  it("lists real service names from the active revision snapshot (array shape, not indices)", async () => {
    const user = userEvent.setup({ pointerEventsCheck: 0 });
    apiMocks.listRevisions.mockResolvedValue({
      revisions: [{ id: "rev-1", seq: "2", status: "active" }],
    });
    apiMocks.getRevisionSpec.mockResolvedValue({
      revision_id: "rev-1",
      compose: JSON.stringify({
        services: [{ name: "web" }, { name: "worker" }],
      }),
    });

    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <MemoryRouter initialEntries={["/apps/foo/logs"]}>
        <QueryClientProvider client={client}>
          <Routes>
            <Route path="/apps/:name/logs" element={<AppLogsPage />} />
          </Routes>
        </QueryClientProvider>
      </MemoryRouter>,
    );
    await waitFor(() => expect(apiMocks.getRevisionSpec).toHaveBeenCalled());

    await user.click(screen.getByRole("combobox", { name: "Service" }));
    expect(
      await screen.findByRole("option", { name: "web" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("option", { name: "worker" })).toBeInTheDocument();
    // 修复前：Object.keys 把数组当 map 解析，下标「0」「1」冒充服务名。
    expect(
      screen.queryByRole("option", { name: "0" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("option", { name: "1" }),
    ).not.toBeInTheDocument();
  });
});
