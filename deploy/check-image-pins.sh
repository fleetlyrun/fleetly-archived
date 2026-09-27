#!/bin/sh
# deploy/check-image-pins.sh — 容器镜像钉 digest 门禁（T0-V2.3 供应链）。
#
# 背景：zane-ops 的 CI 用维护者个人 fork 镜像 + canary 可变 tag（供应链反面
# 教材，docs/research/2026-09-20-zane-ops-comparison.md R7/A6）。本仓 S20-F1
# 已把 GitHub Actions 钉 commit SHA；本门禁把同一纪律扩展到平台脚本/workflow
# 引用的容器镜像：**无 digest 的平台镜像引用即红**。形态要求
# `image:tag@sha256:<64hex>`（tag 保留作可读性，digest 为准；多架构 index
# digest，amd64/arm64 通吃）。digest 解析/复验与离线预拉见
# docs/runbooks/image-prepull.md。
#
# 用法：
#   sh deploy/check-image-pins.sh            # 扫描既定范围（默认），有违规则非零退出
#   sh deploy/check-image-pins.sh -l         # 列表模式：打印全部引用与钉定状态（恒零退出）
#   sh deploy/check-image-pins.sh -a FILE F… # 换豁免清单（负路径自证用临时清单）
#   sh deploy/check-image-pins.sh FILE...    # 只扫指定文件（负路径自证用）
#
# 依赖：POSIX sh + GNU grep（-E/-o/-n/-H）。刻意不用 awk——本仓脚本要求在
# Windows Git Bash（本机无 awk）与 CI ubuntu 双端可跑。
#
# 扫描范围（默认；与钉 digest 改造范围一致）：
#   deploy/*.sh（本脚本除外）
#   deploy/Dockerfile*
#   deploy/testdata/*/Dockerfile*
#   .github/workflows/*.yml
# 范围外（本票禁改/不属平台脚本面）：e2e/**、console/**、docs/**。
#
# 识别口径（行级上下文锚定，尽力而为的文本门禁）：
#   - Dockerfile 的 FROM 行（scratch 跳过；**FROM 行不套用「tag 非纯数字」
#     噪声过滤**——postgres:16/redis:7 这类纯数字 tag 是真实镜像引用，
#     FROM 上下文不可能是时刻/端口映射噪声；2026-09-27 IMPL-DB-0 盲区修复）
#   - 含 `docker pull|run|create|build|push|service|compose` 的命令行
#   - 变量赋值：名含 IMAGE/IMG/TAG 的 `X=…`（sh）与 `X: …`（workflow env/key）
#   - workflow YAML 列表项整项为一个 name:tag[@sha256:…] 字面量（engine 矩阵）
#   - `image:` 键（compose/service 风格预留）
#   - 注释行（含行内 ` #` 注释尾）不参与
# 引用正则要求 name 含字母（排除 127.0.0.1:8080 host:port）、tag 非纯数字
# （排除 16:23 时刻/端口映射噪声；FROM 行豁免——见上）。
# 已知盲区（记录于 runbook，不视为阻断缺陷）：经间接拼装的引用（如
# "${REGISTRY}/${NAME}:${TAG}" 组件化写法）、printf 动态生成的 Dockerfile、
# `COPY --from=<外部镜像>` 行（外部镜像引用请以 FROM stage 声明，落进本
# 扫描口径——IMPL-DB-0 先例）、不含 IMAGE/IMG/TAG 字样的赋值变量名。
#
# 豁免清单：deploy/image-pin-allowlist.txt——非 `#` 开头、非空的每行是一个
# 固定子串（grep -F 语义），命中 `<路径>:<引用>` 即豁免；只收「确需可变
# tag」的引用（如故意验证「可变 tag 被拒」的负路径用例），必须注释理由。

set -u

SELF="deploy/check-image-pins.sh"
ALLOWLIST="deploy/image-pin-allowlist.txt"
LIST_ONLY=0

usage() {
    echo "usage: sh $SELF [-l] [-a ALLOWLIST_FILE] [FILE...]"
    echo "  -l              list mode: print every reference and pin state (always exits 0)"
    echo "  -a ALLOWLIST    override exemption list path (negative-path self-test with a temp list)"
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        -l) LIST_ONLY=1 ;;
        -a) [ $# -ge 2 ] || usage; ALLOWLIST=$2; shift ;;
        -h|--help) usage ;;
        -*) usage ;;
        *) break ;;
    esac
    shift
done

# ── 默认扫描集 ────────────────────────────────────────────────────────────
# 显式给了文件参数就只扫这些；否则按既定范围收集（跳过本脚本自身）。
if [ $# -gt 0 ]; then
    FILES="$*"
else
    FILES=""
    for f in deploy/*.sh; do
        [ "$f" = "$SELF" ] && continue
        FILES="$FILES $f"
    done
    for f in deploy/Dockerfile* deploy/testdata/*/Dockerfile*; do
        [ -f "$f" ] || continue
        FILES="$FILES $f"
    done
    for f in .github/workflows/*.yml; do
        [ -f "$f" ] || continue
        FILES="$FILES $f"
    done
fi

[ -f "$ALLOWLIST" ] || { echo "check-image-pins: allowlist file missing: $ALLOWLIST" >&2; exit 2; }

TMP_CAND="$(mktemp "${TMPDIR:-/tmp}/check-image-pins.cand.XXXXXX")" || exit 2
trap 'rm -f "$TMP_CAND"' EXIT
trap 'rm -f "$TMP_CAND"; exit 130' INT TERM

# ── 第一遍：行级上下文锚定，收集候选行 ────────────────────────────────────
# 候选上下文（ERE，逐行）：
#   1. Dockerfile FROM 行
#   2. docker 子命令行（pull/run/create/build/push/service/compose）
#   3. 变量赋值：名含 IMAGE/IMG/TAG（sh 的 X= 与 yml 的 X:）
#   4. yml 列表项整项 = name:tag[@sha256:hex]（engine 矩阵风格）
#   5. yml `image:` 键（compose/service 风格预留）
CTX_RE='^[[:space:]]*FROM[[:space:]]|docker[[:space:]]+(pull|run|create|build|push|service|compose)[[:space:]]|^[[:space:]]*[A-Za-z_][A-Za-z0-9_-]*(IMAGE|IMG|TAG)[A-Za-z0-9_-]*[=:]|^[[:space:]]*-[[:space:]]+[A-Za-z0-9][A-Za-z0-9._/-]*:[A-Za-z0-9][A-Za-z0-9._-]*(@sha256:[0-9a-fA-F]+)?[[:space:]]*$|^[[:space:]]*image:[[:space:]]'
# 引用形态（ERE）：name:tag + 可选 @sha256:hex 尾——digest 尾随匹配捕获，
# 供钉定判定（name 须含字母、tag 非纯数字的噪声过滤在提取后做）。
REF_RE='[A-Za-z0-9][A-Za-z0-9._/-]*:[A-Za-z0-9][A-Za-z0-9._-]*(@sha256:[0-9a-fA-F]+)?'

nfiles=0
for f in $FILES; do
    [ -f "$f" ] || { echo "check-image-pins: not a file: $f" >&2; exit 2; }
    nfiles=$((nfiles + 1))
    grep -HnE "$CTX_RE" -- "$f" >> "$TMP_CAND"
    rc=$?
    if [ "$rc" -ge 2 ]; then
        echo "check-image-pins: grep failed on $f (exit $rc)" >&2
        exit 2
    fi
done
if [ "$nfiles" -eq 0 ]; then
    echo "check-image-pins: no file matched the scan set" >&2
    exit 2
fi

# ── 第二遍：候选行内提取引用、钉定判定、豁免与报告 ────────────────────────
violations=0
pinned_total=0
exempt_total=0
while IFS= read -r cand; do
    [ -n "$cand" ] || continue
    # grep -Hn 输出 `path:lineno:content`；path 可能含冒号（Windows 盘符
    # C:/…），故逐段吞并到第一个纯数字段为止（段含 / . 字母即并入 path）。
    path=${cand%%:*}
    rest=${cand#*:}
    lineno=''
    while :; do
        seg=${rest%%:*}
        case "$seg" in
            ''|*[!0-9]*)
                rest=${rest#*:}
                path="$path:$seg"
                ;;
            *)
                lineno=$seg
                break
                ;;
        esac
    done
    content=${rest#*:}
    # 注释行跳过；行内 ` #` 注释尾剥掉（sh/Dockerfile/YAML 皆同风格）。
    trimmed=${content#"${content%%[! 	]*}"}
    case "$trimmed" in '#'*) continue ;; esac
    content=${content%%' #'*}

    # 提取本行全部 name:tag[@sha256:hex]（引用内无空白，for 拆分安全）。
    matches=$(printf '%s\n' "$content" | grep -oE "$REF_RE") || matches=""
    for m in $matches; do
        # 拆 @sha256: 尾（若有）。
        base=${m%%'@sha256:'*}
        state=UNPINNED
        if [ "$base" != "$m" ]; then
            tail=${m#*'@sha256:'}
            case "$tail" in
                *[!0-9a-fA-F]*) : ;;                       # 非十六进制 → 未钉
                *)
                    if [ "${#tail}" -ge 64 ]; then
                        state=PINNED                        # @sha256: + ≥64 位 hex
                    fi
                    ;;
            esac
        fi
        # 噪声过滤：name 须含字母（排 host:port/IP）且长度 >=2（排卷挂载
        # 规格尾部 `-v vol:/c:ro` 剥出的 `c:ro`）；tag 非纯数字（排时刻/端口）
        # ——**FROM 行豁免纯数字 tag 过滤**：postgres:16/redis:7 等是真实
        # 镜像引用（IMPL-DB-0 盲区修复；负路径自证 = 未钉 FROM postgres:18）。
        nb=${base%:*}
        tg=${base##*:}
        case "$trimmed" in
            FROM[[:space:]]*) : ;;
            *) case "$tg" in ''|*[!0-9]*) : ;; *) continue ;; esac ;;
        esac
        case "$nb" in *[A-Za-z]*) : ;; *) continue ;; esac
        [ "${#nb}" -ge 2 ] || continue
        # scratch 非 registry 引用。
        [ "$base" = "scratch" ] && continue

        if [ "$state" = PINNED ]; then
            pinned_total=$((pinned_total + 1))
            [ "$LIST_ONLY" -eq 1 ] && echo "pinned   $path:$lineno  $m"
            continue
        fi
        # UNPINNED：查豁免清单（固定子串对 `路径:引用` 匹配）。
        key="$path:$m"
        exempt=""
        while IFS= read -r entry; do
            case "$entry" in
                ''|'#'*) continue ;;
            esac
            if printf '%s\n' "$key" | grep -q -F -- "$entry"; then
                exempt="$entry"
                break
            fi
        done < "$ALLOWLIST"
        if [ -n "$exempt" ]; then
            exempt_total=$((exempt_total + 1))
            [ "$LIST_ONLY" -eq 1 ] && echo "exempt   $path:$lineno  $m  (allowlist: $exempt)"
            continue
        fi
        if [ "$LIST_ONLY" -eq 1 ]; then
            echo "UNPINNED $path:$lineno  $m"
            continue
        fi
        if [ "$violations" -eq 0 ]; then
            echo "check-image-pins: image reference(s) WITHOUT digest (required format: image:tag@sha256:<64hex>):"
        fi
        echo "::error file=$path line=$lineno::container image reference without digest: $m"
        violations=$((violations + 1))
    done
done < "$TMP_CAND"

if [ "$LIST_ONLY" -eq 1 ]; then
    echo "check-image-pins: $pinned_total pinned, $exempt_total exempt (list mode always exits 0)"
    exit 0
fi

if [ "$violations" -gt 0 ]; then
    echo "check-image-pins: FAILED — $violations unpinned reference(s) ($pinned_total pinned, $exempt_total exempt)"
    echo "check-image-pins: pin to image:tag@sha256:<digest>, or add to $ALLOWLIST with a documented reason"
    exit 1
fi
echo "check-image-pins: OK — $pinned_total image reference(s) digest-pinned, $exempt_total exempt"
exit 0
