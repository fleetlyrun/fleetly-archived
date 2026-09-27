// D4-② types 生成化：合并 console 消费的 server/v1 swagger 清单 → 经
// openapi-typescript 生成 src/api/schema.d.ts（`pnpm gen:api`，package.json）。
//
// 输入是 protoc-gen-swagger 的 Swagger 2.0 文档（每 proto 服务一份）；console
// 的 REST 消费面跨多份文件，openapi-typescript 一次只吃一份 spec，故先在
// 内存里合并 paths/definitions（同名 definition 出现在多份文件时断言深度
// 相等后去重——shared 错误信封等公共消息必然重复）。openapi-typescript v7
// 只吃 OpenAPI 3.x，合并后的 Swagger 2.0 文档经 swagger2openapi 就地升格。
//
// CI 门禁（pr.yml console job）：再生成后 `git diff --exit-code` 断言无漂移。

import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import openapiTS, { astToString } from "openapi-typescript";
import converter from "swagger2openapi";

const here = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(here, "..", "..");
const outPath = path.resolve(here, "..", "src", "api", "schema.d.ts");

// console 实际消费的服务面（endpoints.ts / streams.ts 的端点来源 proto）。
// 注意：users.swagger.json 不在清单——S2 取舍沿用（W2-S5 复核）：其
// SetRegistration 挂载在同一路径 /v1/auth/registration（与 auth.swagger.json
// 的 GET 路径键重复，合并器按路径键整体断言 duplicate，方法级互补不豁免），
// 故用户管理面的投影类型（UserView 等）在 types.ts 手写并注明同源
//（W3-S3 起含同文件的 audit retention 两 RPC——路径 /v1/audit/retention
// 与 audit.swagger.json 的 /v1/audit 不冲突，但同文件路径键纪律整体豁免）。
// tokens/projects 随 W2-S2 PAT 管理页进清单；teams 随 W2-S5 团队设置页进
// 清单（成员/角色/邀请管理面）；audit 随 W3-S3 审计浏览页进清单。
const SPEC_FILES = [
  "auth.swagger.json",
  "tokens.swagger.json",
  "teams.swagger.json",
  "projects.swagger.json",
  "audit.swagger.json",
  "apps.swagger.json",
  "deployments.swagger.json",
  // builds 随 P1-6（Builds 台账页）进清单：BuildsService 读面（GetBuild/
  // ListBuilds）是 Builds 页签的数据源。TriggerBuild（POST /v1/builds，
  // admin scope + compose 字节载荷）语义是 CLI 构建入口，Console 不消费
  // ——类型随清单进来但无端点封装。
  "builds.swagger.json",
  "drift.swagger.json",
  // gitkeys 随 P1-8（git push 通道可发现性，2026-09-25 审查 backlog #11）进
  // 清单：GitKeysService 用户自服务三 RPC（Add/List/Remove）是 GitKeys 页的
  // 数据源。scope 登记 read，真授权在 handler 内（gitkeys.go 用户化语义）。
  "gitkeys.swagger.json",
  "revisions.swagger.json",
  "env.swagger.json",
  "domains.swagger.json",
  "logs.swagger.json",
  "metrics.swagger.json",
  "alerting.swagger.json",
  "events.swagger.json",
  "system.swagger.json",
  "placement.swagger.json",
  "cron.swagger.json",
  "database.swagger.json",
  "secrets.swagger.json",
  // configs 随 T 线 OT-3/IMPL-T1-4（Config 资源）进清单：明文配置资源面
  //（Set/List/Get/Remove）是 AppConfigsPage 的数据源（Get 明文回读按 admin
  // 门；写面 admin scope）。
  "configs.swagger.json",
  "notifications.swagger.json",
  "terminal.swagger.json",
];

const merged = {
  swagger: "2.0",
  info: { title: "fleetly console consumed API (merged)", version: "generated" },
  consumes: ["application/json"],
  produces: ["application/json"],
  paths: {},
  definitions: {},
};

for (const file of SPEC_FILES) {
  const spec = JSON.parse(
    await readFile(
      path.join(repoRoot, "genproto", "fleetly", "server", "v1", file),
      "utf8",
    ),
  );
  for (const [p, item] of Object.entries(spec.paths ?? {})) {
    if (merged.paths[p]) {
      throw new Error(`duplicate path across specs: ${p} (${file})`);
    }
    merged.paths[p] = item;
  }
  for (const [name, def] of Object.entries(spec.definitions ?? {})) {
    const prev = merged.definitions[name];
    if (prev !== undefined) {
      // 同名 definition（公共消息，如 v1ErrorResponse）：必须深度一致，
      // 否则 proto 间出现了同 名 不同形 的漂移。
      if (JSON.stringify(prev) !== JSON.stringify(def)) {
        throw new Error(`conflicting definition: ${name} (${file})`);
      }
      continue;
    }
    merged.definitions[name] = def;
  }
}

// Swagger 2.0 → OpenAPI 3.x（openapi-typescript v7 不再收 2.0）。
const { openapi } = await converter.convertObj(merged, {
  patch: true, // 修 protoc-gen-swagger 的轻微不规范处（缺 operationId 唯一性等）
  warnOnly: true,
  resolve: false,
});

const ast = await openapiTS(openapi);
const banner = `// 本文件由 openapi-typescript 从 genproto/fleetly/server/v1/*.swagger.json 生成
// （console/scripts/gen-api.mjs，\`pnpm gen:api\`）——不要手改；proto 变更后
// 重新生成并提交。CI（pr.yml console job）以"再生成无 diff"门禁拦截漂移。
// 字段名/类型语义：UseProtoNames（snake_case 声明名）+ proto3 JSON 映射
// （int64 → 字符串；EmitUnpopulated=false → 零值字段缺省，全部属性可选）。

`;
await mkdir(path.dirname(outPath), { recursive: true });
await writeFile(outPath, banner + astToString(ast) + "\n", "utf8");
process.stdout.write(`wrote ${path.relative(repoRoot, outPath)}\n`);
