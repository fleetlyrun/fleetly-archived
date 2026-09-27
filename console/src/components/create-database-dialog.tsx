// 创建数据库对话框（自 DatabasesPage 抽出的可复用组件，2026-09-25 审查
// §6 建议 1：项目详情页 Databases 卡同享创建 CTA）。模板 + 名 + 可选限额
// 的表单原样保留；目标项目改为显式 prop——消费方传限定形 team/prj，对话框
// 内明示目标（审查发现的「对话框不显示目标项目」可发现性缺口在此收口）；
// prop 缺省回落服务端缺省语义（调用者个人队 default 项目）。

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";

import { createDatabase } from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

// 模板选项（与 server 注册表词表一致；IMPL-DB-1 增两 PG 条目——
// postgres-18 官方镜像与 percona-postgresql-18 发行版面（含 pgvector））。
const TEMPLATES = [
  { value: "postgres-16", label: "postgres-16" },
  { value: "postgres-18", label: "postgres-18" },
  { value: "percona-postgresql-18", label: "percona-postgresql-18" },
  { value: "redis-7", label: "redis-7" },
  { value: "mysql-8.4", label: "mysql-8.4" },
  { value: "mongodb-8.0", label: "mongodb-8.0" },
];

const NAME_PATTERN = /^[a-z0-9][a-z0-9_-]*$/;

export function CreateDatabaseDialog({
  open,
  onOpenChange,
  projectRef = "",
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  /** 目标项目限定形 team/prj（请求 project 透传 + 对话框内明示）；空 = 服务端缺省。 */
  projectRef?: string;
}) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [template, setTemplate] = useState("postgres-16");
  const [cpu, setCpu] = useState("");
  const [memoryGiB, setMemoryGiB] = useState("");
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  const nameOk = NAME_PATTERN.test(name);
  const createMutation = useMutation({
    mutationFn: () => {
      const cpuN = Number(cpu);
      const memN = Number(memoryGiB);
      return createDatabase({
        name,
        template,
        // 目标项目（限定形 team/prj）；空 = 服务端缺省（调用者个人队
        // default 项目）。目标在对话框内显式展示（见 Target project 行）。
        project: projectRef || undefined,
        limits:
          cpu.trim() !== "" || memoryGiB.trim() !== ""
            ? {
                cpu_seconds: cpu.trim() !== "" && !Number.isNaN(cpuN) ? cpuN : 0,
                memory_bytes:
                  memoryGiB.trim() !== "" && !Number.isNaN(memN)
                    ? String(Math.round(memN * 1024 * 1024 * 1024))
                    : undefined,
              }
            : undefined,
      });
    },
    onSuccess: () => {
      setError(null);
      setName("");
      setCpu("");
      setMemoryGiB("");
      void queryClient.invalidateQueries({ queryKey: ["databases"] });
      onOpenChange(false);
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (nameOk) createMutation.mutate();
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent data-testid="database-create-dialog">
        <DialogHeader>
          <DialogTitle>Create database</DialogTitle>
          <DialogDescription>
            A managed instance is created immediately (status provisioning) and
            converges to ready once the engine health gate passes. Credentials
            are generated once and stored encrypted; they are injected into
            referencing apps as <code>FLEETLY_DB_*</code> variables.
          </DialogDescription>
        </DialogHeader>
        <form className="space-y-4" onSubmit={onSubmit}>
          <div className="space-y-1.5">
            <Label>Target project</Label>
            <p
              data-testid="database-target-project"
              className="font-mono text-xs text-muted-foreground"
            >
              {projectRef || "default (your personal team)"}
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="database-name">Name</Label>
            <Input
              id="database-name"
              data-testid="database-name-input"
              className="font-mono text-xs"
              placeholder="pg-prod"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
            <p className="text-xs text-muted-foreground">
              lowercase letters, digits, - and _ (must start alphanumeric).
            </p>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="database-template">Template</Label>
            <Select value={template} onValueChange={setTemplate}>
              <SelectTrigger id="database-template" data-testid="database-template-select" className="w-56">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {TEMPLATES.map((t) => (
                  <SelectItem key={t.value} value={t.value}>
                    {t.label}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label htmlFor="database-cpu">CPU limit (cores)</Label>
              <Input
                id="database-cpu"
                data-testid="database-cpu-input"
                className="font-mono text-xs"
                placeholder="template default"
                inputMode="decimal"
                value={cpu}
                onChange={(e) => setCpu(e.target.value)}
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="database-memory">Memory limit (GiB)</Label>
              <Input
                id="database-memory"
                data-testid="database-memory-input"
                className="font-mono text-xs"
                placeholder="template default"
                inputMode="decimal"
                value={memoryGiB}
                onChange={(e) => setMemoryGiB(e.target.value)}
              />
            </div>
          </div>
          {error ? (
            <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} />
          ) : null}
          <DialogFooter>
            <Button
              type="submit"
              data-testid="database-create-submit"
              disabled={!nameOk || createMutation.isPending}
            >
              {createMutation.isPending ? "Creating…" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
