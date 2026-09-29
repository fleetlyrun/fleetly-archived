// 库实例详情（E4 managed-databases §4 验收步 9）：标题行（名称 + 模板 +
// 生命周期态徽章 + failed 的 last_error 告警）+ 状态相宜的操作面（挂起/恢
// 复/重试/轮换/升级/删除——合法前置态见设计 §2.3 操作表）+ 连接卡（脱敏
// 投影默认、显式 reveal 展开明文——admin 动作，敏感面已标注）+ 备份卡。

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  CircleAlert,
  Copy,
  Database,
  Eye,
  EyeOff,
  KeyRound,
  Pause,
  Play,
  RotateCw,
  Trash2,
  TrendingUp,
} from "lucide-react";
import { useState } from "react";
import { Link, useParams } from "react-router-dom";

import {
  deleteDatabase,
  getDatabase,
  retryDatabase,
  revealDatabaseCredentials,
  resumeDatabase,
  rotateDatabaseCredentials,
  suspendDatabase,
  upgradeDatabase,
} from "@/api/endpoints";
import { errorEnvelopeFrom } from "@/api/errors";
import { DatabaseBackupsCard } from "@/components/database-backups-card";
import { EnvelopeAlert } from "@/components/envelope-alert";
import { StateBadge } from "@/components/state-badge";
import { Button } from "@/components/ui/button";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
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
import { formatTime } from "@/lib/utils";
import { useIsPlatformAdmin, useTeamCapabilities } from "@/lib/context";
import { useSubjectResolver } from "@/hooks/use-subject-resolver";

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className="text-right font-medium">{value}</span>
    </div>
  );
}

// 合法前置态（设计 §2.3 操作表）——按钮的启用判据唯一真源；非法前置态的
// 点击面在服务端也以 409 E_STATE_VERSION_CONFLICT 诚实拒绝（双保险，本处
// 只是不給不可行动作一个可点的按钮）。
const ACTIONS: Record<string, string[]> = {
  suspend: ["ready", "degraded"],
  resume: ["paused"],
  retry: ["failed"],
  rotate: ["ready", "degraded", "paused"],
  upgrade: ["ready", "degraded", "paused"],
  backup: ["ready", "degraded"],
  restore: ["ready", "degraded"],
  delete: ["provisioning", "ready", "failed", "degraded", "paused"],
};

type ConfirmKind = "rotate" | "upgrade" | "delete" | null;

function ConfirmDialog({
  kind,
  name,
  deleteVolumes,
  onDeleteVolumesChange,
  onClose,
}: {
  kind: Exclude<ConfirmKind, null>;
  name: string;
  deleteVolumes: boolean;
  onDeleteVolumesChange: (v: boolean) => void;
  onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);
  const [result, setResult] = useState<string>("");
  const confirmOk = confirm === name;

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["database", name] });
    void queryClient.invalidateQueries({ queryKey: ["databases"] });
  };

  const rotateMutation = useMutation({
    mutationFn: () => rotateDatabaseCredentials(name, name),
    onSuccess: (r) => {
      setError(null);
      const apps = r.redeployed_apps ?? [];
      setResult(
        apps.length > 0
          ? `Credentials rotated. Referencing apps requeued for redeploy: ${apps.join(", ")}.`
          : "Credentials rotated (no referencing apps).",
      );
      invalidate();
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });
  const upgradeMutation = useMutation({
    mutationFn: () => upgradeDatabase(name, name),
    onSuccess: () => {
      setError(null);
      setResult("Upgrade accepted — the pre-upgrade backup gate runs first; progress surfaces via events.");
      invalidate();
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });
  const deleteMutation = useMutation({
    mutationFn: () => deleteDatabase(name, { confirm: name, delete_volumes: deleteVolumes }),
    onSuccess: () => {
      setError(null);
      invalidate();
      onClose();
    },
    onError: (err) => setError(errorEnvelopeFrom(err)),
  });

  const pending =
    rotateMutation.isPending || upgradeMutation.isPending || deleteMutation.isPending;

  const titles: Record<Exclude<ConfirmKind, null>, string> = {
    rotate: "Rotate credentials",
    upgrade: "Upgrade database",
    delete: "Delete database",
  };
  const descriptions: Record<Exclude<ConfirmKind, null>, React.ReactNode> = {
    rotate: (
      <>
        Rotation is destructive and irreversible: the password is replaced and{" "}
        <strong>every referencing app is redeployed automatically</strong> (its
        old password stops working the moment rotation completes). Existing
        connections must reconnect with the new credential.
      </>
    ),
    upgrade: (
      <>
        Upgrade rebuilds the instance on the current template image with a
        downtime window. A verified pre-upgrade backup is taken first; if the
        new version fails its health gate the platform rolls the image digest
        back.
      </>
    ),
    delete: (
      <>
        The instance is tombstoned and its managed service is reaped in the
        background. By default <strong>the data volume is kept as orphaned</strong>{" "}
        (recoverable by hand) — tick the box below to irreversibly delete the
        data instead. Referencing apps must be removed first.
      </>
    ),
  };

  function submit() {
    if (kind === "rotate") rotateMutation.mutate();
    else if (kind === "upgrade") upgradeMutation.mutate();
    else deleteMutation.mutate();
  }

  return (
    <Dialog open onOpenChange={(v) => (v ? undefined : onClose())}>
      <DialogContent data-testid={`database-${kind}-dialog`}>
        <DialogHeader>
          <DialogTitle>{titles[kind]}</DialogTitle>
          <DialogDescription>{descriptions[kind]}</DialogDescription>
        </DialogHeader>
        {kind === "delete" ? (
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              data-testid="database-delete-volumes-toggle"
              className="h-4 w-4 accent-foreground"
              checked={deleteVolumes}
              onChange={(e) => onDeleteVolumesChange(e.target.checked)}
            />
            Delete the data volume irreversibly (default: keep it as orphaned)
          </label>
        ) : null}
        <div className="space-y-1.5">
          <Label htmlFor={`database-${kind}-confirm`}>
            Type the instance name <span className="font-mono">{name}</span> to confirm
          </Label>
          <Input
            id={`database-${kind}-confirm`}
            data-testid={`database-${kind}-confirm-input`}
            className="font-mono text-xs"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
        </div>
        {error ? (
          <EnvelopeAlert code={error.code} message={error.message} suggestion={error.suggestion} />
        ) : null}
        {result ? (
          <p className="text-xs text-emerald-600 dark:text-emerald-400" data-testid={`database-${kind}-result`}>
            {result}
          </p>
        ) : null}
        <DialogFooter>
          <Button
            variant={kind === "delete" ? "destructive" : "default"}
            data-testid={`database-${kind}-submit`}
            disabled={!confirmOk || pending || result !== ""}
            onClick={submit}
          >
            {pending ? "Working…" : "Confirm"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** 连接卡：脱敏投影常显 + 显式 reveal（admin 面——取到的明文只在内存/前台，隐藏即弃）。
 * 写面双门（P0-3 双门 + 角色门 2026-09-25 走查）：reveal 钮按 canAdminResources
 * （admin+）渲染——此前只门了平台管理员，viewer/developer 见到点了必 403 的
 * 假按钮；平台管理员说明态保留，非管理员成员渲染零变化。 */
function ConnectionCard({ name }: { name: string }) {
  const platformReadonly = useIsPlatformAdmin();
  const { canAdminResources } = useTeamCapabilities();
  const query = useQuery({ queryKey: ["database", name], queryFn: () => getDatabase(name) });
  const conn = query.data?.database?.connection;
  const [revealed, setRevealed] = useState<{ password: string; url: string } | null>(null);
  const [copied, setCopied] = useState(false);
  const [revealError, setRevealError] = useState<string>("");

  const revealMutation = useMutation({
    mutationFn: () => revealDatabaseCredentials(name),
    onSuccess: (r) => {
      setRevealError("");
      setRevealed({ password: r.password ?? "", url: r.url ?? "" });
    },
    onError: (err) => setRevealError(errorEnvelopeFrom(err).message ?? "reveal failed"),
  });

  const copy = async () => {
    if (!revealed) return;
    try {
      await navigator.clipboard.writeText(revealed.password);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // 剪贴板不可用（非安全上下文/权限）：静默——明文本就在面板上。
    }
  };

  return (
    <Card data-testid="database-connection-card">
      <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
        <KeyRound aria-hidden className="h-4 w-4 text-muted-foreground" />
        <CardTitle className="text-sm font-semibold">Connection</CardTitle>
        <CardDescription className="ml-auto text-xs">
          Reachable only from referencing apps on the shared network (host = instance name)
        </CardDescription>
      </CardHeader>
      <CardContent className="space-y-3 pt-4">
        {conn ? (
          <>
            <Field label="Host" value={<code className="text-xs">{conn.host}</code>} />
            <Field label="Port" value={<code className="text-xs">{conn.port}</code>} />
            {/* EmitUnpopulated=true 下 Redis 的 user/database 显式输出空串
                （此前缺省不输出）——真值判定跳过空值行，形态与旧口径一致。 */}
            {conn.user ? (
              <Field label="User" value={<code className="text-xs">{conn.user}</code>} />
            ) : null}
            {conn.database ? (
              <Field label="Database" value={<code className="text-xs">{conn.database}</code>} />
            ) : null}
            <Field label="URL" value={<code className="text-xs">{conn.url}</code>} />
            <Field
              label="Password"
              value={
                revealed ? (
                  <span className="flex items-center gap-2">
                    <code className="text-xs" data-testid="database-revealed-password">{revealed.password}</code>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-6 w-6"
                      aria-label="Copy password"
                      data-testid="database-copy-password"
                      onClick={() => void copy()}
                    >
                      {copied ? <Check aria-hidden className="h-3.5 w-3.5" /> : <Copy aria-hidden className="h-3.5 w-3.5" />}
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-6 w-6"
                      aria-label="Hide password"
                      onClick={() => {
                        setRevealed(null);
                        setCopied(false);
                      }}
                    >
                      <EyeOff aria-hidden className="h-3.5 w-3.5" />
                    </Button>
                  </span>
                ) : (
                  <span className="flex items-center gap-2">
                    <code className="text-xs text-muted-foreground">{conn.password_fingerprint ?? "—"}</code>
                    <span className="text-xs text-muted-foreground">(fingerprint)</span>
                    {!canAdminResources ? null : (
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7"
                        data-testid="database-reveal-button"
                        disabled={revealMutation.isPending}
                        onClick={() => revealMutation.mutate()}
                      >
                        <Eye aria-hidden className="h-3.5 w-3.5" />
                        Reveal
                      </Button>
                    )}
                  </span>
                )
              }
            />
            {platformReadonly ? (
              // P0-3：平台管理员只读——说明行替换「Admin action」段（该段描
              // 述的正是其不可用的动作）。
              <p className="text-xs text-muted-foreground" data-testid="platform-readonly-note">
                Platform administrators have read-only access to resources
                (separation of duties). Reveal the credential from the CLI with
                a machine token (<code>fleetly databases reveal</code>), or ask
                a team owner for a member role.
              </p>
            ) : (
              <p className="text-xs text-muted-foreground">
                Admin action: revealing shows the plaintext password in the UI and
                is recorded in the audit log. The masked URL never leaves storage.
              </p>
            )}
            {revealError ? (
              <p className="text-xs text-red-600 dark:text-red-400">{revealError}</p>
            ) : null}
          </>
        ) : (
          <p className="text-sm text-muted-foreground">Loading connection…</p>
        )}
      </CardContent>
    </Card>
  );
}

export function DatabaseDetailPage() {
  const { name = "" } = useParams();
  const queryClient = useQueryClient();
  // 生命周期写面双门（2026-09-25 走查）：资源面写钮按 canAdminResources
  //（admin+）渲染——此前只门了平台管理员（P0-3 单门），viewer/developer
  // 见到整排 enabled 假按钮（点了服务端 403）。平台管理员说明态保留；
  // 非管理员的普通成员给成员角色语义的说明卡。
  const platformReadonly = useIsPlatformAdmin();
  const { canAdminResources } = useTeamCapabilities();
  // placement/volume 节点 ID 可读化（W2-7）。
  const resolveSubject = useSubjectResolver();
  const query = useQuery({
    queryKey: ["database", name],
    queryFn: () => getDatabase(name),
    refetchInterval: 5000,
  });
  const [confirmKind, setConfirmKind] = useState<ConfirmKind>(null);
  const [deleteVolumes, setDeleteVolumes] = useState(false);
  const [actionError, setActionError] = useState<ReturnType<typeof errorEnvelopeFrom> | null>(null);

  const db = query.data?.database;
  const status = db?.status ?? "";

  const afterAction = () => {
    setActionError(null);
    void queryClient.invalidateQueries({ queryKey: ["database", name] });
    void queryClient.invalidateQueries({ queryKey: ["databases"] });
  };
  const suspendMutation = useMutation({
    mutationFn: () => suspendDatabase(name),
    onSuccess: afterAction,
    onError: (err) => setActionError(errorEnvelopeFrom(err)),
  });
  const resumeMutation = useMutation({
    mutationFn: () => resumeDatabase(name),
    onSuccess: afterAction,
    onError: (err) => setActionError(errorEnvelopeFrom(err)),
  });
  const retryMutation = useMutation({
    mutationFn: () => retryDatabase(name),
    onSuccess: afterAction,
    onError: (err) => setActionError(errorEnvelopeFrom(err)),
  });

  if (query.isError) {
    const envelope = errorEnvelopeFrom(query.error);
    return (
      <div className="space-y-4" data-testid="database-detail-page">
        <EnvelopeAlert
          code={envelope.code}
          message={envelope.message}
          suggestion={envelope.suggestion}
          docs={envelope.docs}
        />
      </div>
    );
  }

  const can = (kind: keyof typeof ACTIONS) => ACTIONS[kind].includes(status);
  const terminalish = status === "deleting" || status === "deleted";

  return (
    <div className="space-y-4" data-testid="database-detail-page">
      <div className="flex flex-wrap items-center gap-3">
        <span className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg border bg-muted/40">
          <Database aria-hidden className="h-5 w-5 text-muted-foreground" />
        </span>
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2.5">
            <h1 className="truncate font-mono text-xl font-semibold tracking-tight">{name}</h1>
            {db ? <StateBadge state={status} /> : null}
            <span className="rounded-md border px-1.5 py-0.5 text-xs text-muted-foreground">
              {db?.template}
            </span>
          </div>
          {db ? (
            <p className="text-xs text-muted-foreground">
              updated {formatTime(db.updated_at)} · created {formatTime(db.created_at)}
            </p>
          ) : null}
        </div>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          {canAdminResources ? (
            <>
              <Button
                variant="outline"
                size="sm"
                data-testid="database-suspend-button"
                disabled={!can("suspend") || suspendMutation.isPending}
                title={can("suspend") ? "Scale to zero (volumes kept); referencing apps lose connectivity" : "Suspend needs ready/degraded"}
                onClick={() => suspendMutation.mutate()}
              >
                <Pause aria-hidden className="h-3.5 w-3.5" />
                Suspend
              </Button>
              <Button
                variant="outline"
                size="sm"
                data-testid="database-resume-button"
                disabled={!can("resume") || resumeMutation.isPending}
                title={can("resume") ? "Reconverge to ready" : "Resume applies to paused instances"}
                onClick={() => resumeMutation.mutate()}
              >
                <Play aria-hidden className="h-3.5 w-3.5" />
                Resume
              </Button>
              <Button
                variant="outline"
                size="sm"
                data-testid="database-retry-button"
                disabled={!can("retry") || retryMutation.isPending}
                title={can("retry") ? "Retry convergence from the failed scene" : "Retry applies to failed instances"}
                onClick={() => retryMutation.mutate()}
              >
                <RotateCw aria-hidden className="h-3.5 w-3.5" />
                Retry
              </Button>
              {db?.upgrade_available ? (
                <Button
                  variant="outline"
                  size="sm"
                  data-testid="database-upgrade-button"
                  disabled={!can("upgrade") || terminalish}
                  title={can("upgrade") ? "Controlled rebuild on the current template image" : "Upgrade needs ready/degraded/paused"}
                  onClick={() => setConfirmKind("upgrade")}
                >
                  <TrendingUp aria-hidden className="h-3.5 w-3.5" />
                  Upgrade
                </Button>
              ) : null}
              <Button
                variant="outline"
                size="sm"
                data-testid="database-rotate-button"
                disabled={!can("rotate")}
                title={can("rotate") ? "Destructive credential rotation (referencing apps are redeployed)" : "Rotate needs ready/degraded/paused"}
                onClick={() => setConfirmKind("rotate")}
              >
                <KeyRound aria-hidden className="h-3.5 w-3.5" />
                Rotate
              </Button>
              <Button
                variant="destructive"
                size="sm"
                data-testid="database-delete-button"
                disabled={!can("delete")}
                title={can("delete") ? "Tombstone the instance (volume kept by default)" : "Instance is already deleting/deleted"}
                onClick={() => setConfirmKind("delete")}
              >
                <Trash2 aria-hidden className="h-3.5 w-3.5" />
                Delete
              </Button>
            </>
          ) : null}
        </div>
      </div>

      {platformReadonly ? (
        // P0-3：操作面原位说明（生命周期 CLI 等价命令齐备——fleetly
        // databases <suspend|resume|retry|rotate|upgrade|delete|backup|restore>）。
        <Card className="border-dashed">
          <CardContent
            className="p-4 text-sm text-muted-foreground"
            data-testid="platform-readonly-note"
          >
            Platform administrators have read-only access to resources
            (separation of duties). Manage this database instance from the CLI
            with a machine token (<code>fleetly databases</code> suspend, rotate,
            backup, restore, …), or ask a team owner for a member role.
          </CardContent>
        </Card>
      ) : !canAdminResources ? (
        // 成员角色门（2026-09-25 走查）：viewer/developer 此前见到整排
        // enabled 假按钮——原位说明卡（platform-readonly-note 同形态、
        // 成员角色语义文案）。
        <Card className="border-dashed">
          <CardContent
            className="p-4 text-sm text-muted-foreground"
            data-testid="database-role-note"
          >
            Database lifecycle actions require the admin role in this project.
          </CardContent>
        </Card>
      ) : null}

      {db?.last_error ? (
        <div
          data-testid="database-last-error"
          className="flex items-start gap-2 rounded-md border border-red-500/40 bg-red-500/5 p-3 text-xs text-red-800 dark:text-red-300"
        >
          <CircleAlert aria-hidden className="mt-0.5 h-3.5 w-3.5 shrink-0" />
          <span>
            Last convergence failure: {db.last_error}
            {status === "failed" ? " — retry re-runs convergence (the scene is preserved)." : ""}
          </span>
        </div>
      ) : null}
      {terminalish ? (
        <p className="text-xs text-muted-foreground">
          This instance is {status}; actions are settled.{" "}
          <Link to="/databases" className="underline">Back to the list</Link>.
        </p>
      ) : null}
      {actionError ? (
        <EnvelopeAlert code={actionError.code} message={actionError.message} suggestion={actionError.suggestion} />
      ) : null}

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
            <Database aria-hidden className="h-4 w-4 text-muted-foreground" />
            <CardTitle className="text-sm font-semibold">Instance</CardTitle>
          </CardHeader>
          <CardContent className="space-y-3 pt-4">
            {db ? (
              <>
                <Field label="Status" value={<StateBadge state={status} />} />
                <Field label="Image" value={<code className="text-xs">{db.image_digest}</code>} />
                <Field label="Placement" value={<code className="text-xs">{resolveSubject(db.placement) || "—"}</code>} />
                <Field
                  label="Volume"
                  value={
                    db.volume ? (
                      <span className="text-right">
                        <code className="text-xs">{db.volume.name}</code>
                        <span className="block text-xs text-muted-foreground">
                          {db.volume.status} · node {resolveSubject(db.volume.platform_node_id)}
                        </span>
                      </span>
                    ) : (
                      "—"
                    )
                  }
                />
                <Field
                  label="Limits"
                  value={
                    db.limits?.cpu_seconds == null && db.limits?.memory_bytes == null ? (
                      <span className="text-xs text-muted-foreground">not set (engine defaults)</span>
                    ) : (
                      <span className="text-xs">
                        cpu {db.limits?.cpu_seconds ?? "not set"} · mem{" "}
                        {db.limits?.memory_bytes != null ? `${db.limits.memory_bytes} B` : "not set"}
                      </span>
                    )
                  }
                />
                <Field
                  label="Backup plan"
                  value={
                    !db.backup_plan?.interval_hours && db.backup_plan?.keep == null ? (
                      <span className="text-xs text-muted-foreground">not set (platform default: daily 03:00 UTC, keep 7)</span>
                    ) : (
                      <span className="text-xs">
                        every {db.backup_plan?.interval_hours ?? "—"}h · keep {db.backup_plan?.keep ?? "—"} ·{" "}
                        {String(db.backup_plan?.hour_utc ?? "—")}:00 UTC
                      </span>
                    )
                  }
                />
                <Field
                  label="Credentials updated"
                  value={<span title={formatTime(db.credential_updated_at)} className="text-xs">{formatTime(db.credential_updated_at)}</span>}
                />
              </>
            ) : (
              <p className="text-sm text-muted-foreground">Loading…</p>
            )}
          </CardContent>
        </Card>

        <ConnectionCard name={name} />

        <div className="lg:col-span-2">
          <DatabaseBackupsCard name={name} actionable={can("backup")} />
        </div>
      </div>

      {confirmKind ? (
        <ConfirmDialog
          kind={confirmKind}
          name={name}
          deleteVolumes={deleteVolumes}
          onDeleteVolumesChange={setDeleteVolumes}
          onClose={() => setConfirmKind(null)}
        />
      ) : null}
    </div>
  );
}
