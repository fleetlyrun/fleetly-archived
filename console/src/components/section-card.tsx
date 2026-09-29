// 区块卡：设计语言单点（2026-09-29 IA 重设计，设计
// docs/design/2026-09-29-console-ia-redesign.md §6）——图标+标题+一句描述
// +右上 actions 槽 + border-b 头部的卡片范式。此前该形态以 className 手拼
// 在约 20 处重复（flex-row items-center gap-2 space-y-0 border-b pb-3），
// 新面一律经本组件，存量卡片渐进收编。

import type { LucideIcon } from "lucide-react";
import type { ReactNode } from "react";

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export function SectionCard({
  icon: Icon,
  title,
  description,
  actions,
  children,
  className,
  contentClassName,
  testId,
}: {
  icon?: LucideIcon;
  title: ReactNode;
  description?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  contentClassName?: string;
  /** 根元素 data-testid（测试锚点；Card 不透传任意 props，此处显式承载）。 */
  testId?: string;
}) {
  return (
    <Card className={className} data-testid={testId}>
      <CardHeader className="flex-row items-center gap-2 space-y-0 border-b pb-3">
        {Icon ? (
          <Icon aria-hidden className="h-4 w-4 shrink-0 text-muted-foreground" />
        ) : null}
        <div className="min-w-0 flex-1">
          <CardTitle className="text-sm font-semibold">{title}</CardTitle>
          {description ? (
            <p className="mt-0.5 text-xs text-muted-foreground">{description}</p>
          ) : null}
        </div>
        {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
      </CardHeader>
      <CardContent className={contentClassName ?? "space-y-3 pt-4"}>
        {children}
      </CardContent>
    </Card>
  );
}
