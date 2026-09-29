// Compose 可视化（2026-09-29 IA 重设计，设计 §4.4「实际生效的 compose 文
// 件」）：GetRevisionSpec 的归一化快照（canonical JSON）此前只被解析成
// 服务/diff，从不渲染原文。本组件三形态：
//   - AppComposeCard:Overview 的「Compose」卡（active revision 快照）；
//   - ComposeDialog:Deployments 每行的「View compose」对话框（该部署
//     revision 的快照——历史每一版的实际 compose 可回看）；
//   - ComposeCode:共用的代码块渲染（pretty-print，解析失败回退原文）。
// 如实文案：env 值以 SHA-256 哈希存储（值明文结构性不在快照中，平台无
// 明文）；secrets/configs 是引用不是内联值。

import { useQuery } from "@tanstack/react-query";
import { Check, Copy, FileCode2 } from "lucide-react";
import { useState } from "react";

import {
  getRevisionSpec,
  listRevisions,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import { EmptyState } from "@/components/empty-state";
import { SectionCard } from "@/components/section-card";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

/** 归一化快照 → 可读代码块文本（pretty-print；解析失败回退原文——不伪造）。 */
function composeText(compose: string | undefined): string {
  if (!compose) return "";
  try {
    return JSON.stringify(JSON.parse(compose), null, 2);
  } catch {
    return compose;
  }
}

/** 快照代码块（等宽 + 行内滚动；data-testid=compose-viewer 钉测）。 */
export function ComposeCode({ compose, maxHeightClass }: { compose: string; maxHeightClass?: string }) {
  return (
    <pre
      data-testid="compose-viewer"
      className={
        "overflow-auto rounded-lg border bg-muted/30 p-3 font-mono text-xs leading-relaxed " +
        (maxHeightClass ?? "max-h-[480px]")
      }
    >
      {composeText(compose)}
    </pre>
  );
}

/** 复制按钮（短态反馈 Copied——无 toast 基建，就地换标）。 */
function CopyComposeButton({ compose }: { compose: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <Button
      variant="outline"
      size="sm"
      data-testid="compose-copy"
      onClick={() => {
        void navigator.clipboard.writeText(composeText(compose)).then(() => {
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1500);
        });
      }}
    >
      {copied ? (
        <Check aria-hidden className="h-3.5 w-3.5" />
      ) : (
        <Copy aria-hidden className="h-3.5 w-3.5" />
      )}
      {copied ? "Copied" : "Copy"}
    </Button>
  );
}

/** Overview 的 Compose 卡：active revision 快照（与 Overview 服务清单同源
    同查询——react-query 同 key 去重，零额外请求）。 */
export function AppComposeCard({ app }: { app: string }) {
  const revisionsQuery = useQuery({
    queryKey: ["revisions", app],
    queryFn: () => listRevisions(app),
  });
  const active = (revisionsQuery.data?.revisions ?? []).find(
    (r) => r.status === "active",
  );
  const specQuery = useQuery({
    queryKey: ["revision-spec", app, active?.id],
    queryFn: () => getRevisionSpec(app, active!.id ?? ""),
    enabled: active !== undefined,
  });

  const spec = specQuery.data?.compose;
  const seq = active?.seq;
  return (
    <SectionCard
      icon={FileCode2}
      title="Compose"
      description={
        seq !== undefined
          ? `Effective normalized snapshot of revision #${seq} — env values are stored as SHA-256 hashes; secrets and configs are referenced, not inlined.`
          : "The effective normalized compose snapshot that the platform converges to."
      }
      actions={spec ? <CopyComposeButton compose={spec} /> : undefined}
    >
      {spec ? (
        <ComposeCode compose={spec} />
      ) : revisionsQuery.isSuccess && !active ? (
        <EmptyState
          icon={FileCode2}
          title="No compose deployed yet"
          hint="Deploy a compose file from the Deployments tab — its normalized snapshot will appear here."
        />
      ) : (
        <p className="text-sm text-muted-foreground">Loading…</p>
      )}
      {specQuery.isError ? (
        <p className="text-xs text-red-600 dark:text-red-400">
          {errorEnvelopeFrom(specQuery.error).message}
        </p>
      ) : null}
    </SectionCard>
  );
}

/** Deployments 每行的快照对话框（该部署 revision 的实际 compose）。 */
export function ComposeDialog({
  app,
  deploymentId,
  revisionId,
  open,
  onOpenChange,
}: {
  app: string;
  deploymentId: string;
  revisionId?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const specQuery = useQuery({
    queryKey: ["revision-spec", app, revisionId],
    queryFn: () => getRevisionSpec(app, revisionId ?? ""),
    enabled: open && revisionId !== undefined,
  });
  const spec = specQuery.data?.compose;
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-3xl" data-testid="deployment-compose-dialog">
        <DialogHeader>
          <DialogTitle>Compose — deployment {(deploymentId || "").slice(0, 12)}</DialogTitle>
          <DialogDescription>
            Effective normalized snapshot of this deployment's revision — env
            values are stored as SHA-256 hashes.
          </DialogDescription>
        </DialogHeader>
        {spec ? (
          <div className="space-y-3">
            <ComposeCode compose={spec} maxHeightClass="max-h-[60vh]" />
            <div className="flex justify-end">
              <CopyComposeButton compose={spec} />
            </div>
          </div>
        ) : specQuery.isLoading ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : (
          <p className="text-sm text-muted-foreground">
            Snapshot unavailable for this deployment.
          </p>
        )}
      </DialogContent>
    </Dialog>
  );
}
