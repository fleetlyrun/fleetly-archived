package victorialogs

// VL HTTP 消费面（设计 §2.3 入湖 / §3.1 检索）：hub 批量器的传输后端
//（IngestBulk——ES bulk 形态）与 SearchLogs 的查询后端（Search——LogsQL）。
//
// 端点与形态按 VL v1.52 实测核实（2026-09-21，本机容器真机验证）：
//   - GET  /health → 200 "OK"（回环拨测）；
//   - POST /insert/elasticsearch/_bulk?_stream_fields=app,service,source
//     （NDJSON：每行先 {"create":{}} 动作行、后字段行；_time 接受
//     RFC3339；_msg 为消息；设计原文「ES bulk」）；
//   - GET/POST /select/logsql/query（form：query/limit/offset/start/end/
//     timeout；响应 = JSON 行流，不排序——limit 取 _time 最大的 N 条）。
//
// 注入安全（设计 §3.1 硬性）：keyword/服务名/来源值全部经 EscapeLogsQLPhrase
// 转义后包进双引号字面量短语，用户输入永不裸拼进查询串；服务名/来源另有
// 白名单校验（正负向单测钉住）。流过滤值用 regexp.QuoteMeta 嵌入
// `~"^(…)$"` 锚定正则（白名单字符集之外的字面量同样不可能逃逸）。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fleetlyrun/fleetly/internal/logs"
)

// loopbackBase 是 fleetlyd 侧的 VL 回环基址（D-W5-4：host-mode 回环发布，
// hub 直推与查询都走 127.0.0.1:9428——零公网面）。
func loopbackBase() string {
	return fmt.Sprintf("http://127.0.0.1:%d", IngestPort)
}

// servicePattern 是服务名/应用名的白名单字形（设计 §3.1：`^[a-z0-9-]+$`
// 类既有约束；首位收紧为字母数字，拒绝纯符号形态）。
var servicePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// appPattern 是 app 流过滤值的白名单（v0.3 流标签口径，rbac-teams §4.3）：
// 裸名或三段限定形 `team/prj/app`——slug 段同服务名字表，'/' 为限定分隔符
//（至多两个）；注入字形（引号/管道/空白）仍被结构性挡住。
var appPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9-]*)?$`)

// taskPattern 是任务流选择器的白名单形（DT-5 任务日志面：task 标签 =
// 26 位 Crockford ULID；服务名/ID 之外的形态拒绝，注入安全硬性条款同源）。
var taskPattern = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// allowedSources 是来源白名单词表（S1：container|build；S2 起访问日志
// source=access 进入词表——与采集面同拍放行，诚实边界不预放行）。
var allowedSources = map[string]bool{
	logs.SourceContainer: true,
	logs.SourceBuild:     true,
	logs.SourceAccess:    true,
}

// EscapeLogsQLPhrase 把任意字符串转义为 LogsQL 双引号字面量短语的内容体
//（不含外层引号）：反斜杠 → `\\`、双引号 → `\"`。其余字符（管道、星号、
// 空格、引号变体）在双引号字面量内均为字面语义——VL v1.52 实测：含
// `a"b\c|d*e` 的消息可被转义短语精确命中、不逃逸出短语语义。正负向单测
// 钉住（backend_test.go）。
func EscapeLogsQLPhrase(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// SearchQuery 是 SearchLogs 的查询输入（api 层投影后的形态）。
type SearchQuery struct {
	// Apps/Services/Sources/Tasks 是可选过滤集（空 = 不过滤该维度）。
	Apps     []string
	Services []string
	Sources  []string
	// Tasks 是任务流选择器（DT-5 任务日志面：task 标签 = 任务平台 ID）。
	Tasks []string
	// Keyword 是全文短语（空 = 无关键词过滤）。
	Keyword string
	// Start/End 是时间窗（零值 = 不设界；end 传给 VL 的语义为开区间）。
	Start, End time.Time
	// Limit 是返回上限（调用方已夹紧到 [1,1000]）。
	Limit int
	// Offset 是分页偏移（VL 语义：跳过 _time 最大的 offset 条）。
	Offset int
	// Timeout 是查询执行预算（零值 = 10s 缺省）。
	Timeout time.Duration
}

// LogRow 是 SearchLogs 的单行结果（proto 投影的源形态）。
type LogRow struct {
	At      time.Time
	App     string
	Service string
	Source  string
	// Task 是任务日志归因（DT-5：task 标签 = 任务平台 ID；app 行恒空）。
	Task   string
	Stderr bool
	Msg    string
	// Fields 是 access 行的结构化字段回读（白名单键内取回；container/
	// build 行为 nil）。
	Fields map[string]string
}

// Backend 是 VL 的 HTTP 消费端（无状态；hub 批量器与 SearchLogs 共用）。
// 实现日志管线的 IngestBackend 端口（方向纪律：logs 定义端口、本包实现
// ——logs 不反向感知 VL）。
type Backend struct {
	base string
	hc   *http.Client
	// now 是行时间缺省（时钟注入缝；单测钉死）。
	now func() time.Time
}

// NewBackend 构造回环消费端（hc nil = 缺省客户端——生产形态；VL 故障的
// 失败预算由调用方 ctx 与 streak 面承载，客户端不设整请求超时——批量器
// flush 有自己的预算）。
func NewBackend() *Backend {
	return NewBackendWithBase(loopbackBase())
}

// NewBackendWithBase 以指定基址构造（装配/测试注入缝——生产装配恒用
// NewBackend 的回环形态；nil hc 的缺省客户端语义同 NewBackend）。
func NewBackendWithBase(base string) *Backend {
	return &Backend{base: base, hc: http.DefaultClient, now: time.Now}
}

// Ping 是回环健康拨测（Manager.CheckHealth 的可达性面）。
func (b *Backend) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+HealthPath, nil)
	if err != nil {
		return fmt.Errorf("victorialogs: build health request: %w", err)
	}
	res, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("victorialogs: health probe: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("victorialogs: health probe status %s", res.Status)
	}
	return nil
}

// IngestBulk 实现日志管线 IngestBackend 端口（internal/logs）：把一批已
// 脱敏行以 ES bulk 形态入湖（_stream_fields=app,service,source；行字段
// _time/_msg/app/service/source/stderr——设计 §3.1 行集契约；W5-S2：access
// 行的结构化 Fields 展开为同层行字段（键经 logs.AllowedAccessFieldKey 白名
// 单过滤——method/status/host/path/route/duration_ms/client_ip/deployment_id
// 之外的键不入湖）。非 2xx 一律按失败返回（含响应体前 256B 摘要——批量器
// streak 面的取证材料）。
func (b *Backend) IngestBulk(ctx context.Context, entries []logs.Entry) error {
	if len(entries) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for _, e := range entries {
		buf.WriteString("{\"create\":{}}\n")
		row := bulkRow{
			Time:    e.At.UTC().Format(time.RFC3339Nano),
			Msg:     e.Line,
			App:     e.App,
			Service: e.Service,
			Source:  e.Source,
			Task:    e.Task,
			Stderr:  e.Stderr,
		}
		raw, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("victorialogs: marshal bulk row: %w", err)
		}
		if len(e.Fields) > 0 {
			// 访问行：结构化字段展开为行顶层键（VL 扁平文档模型——嵌套
			// 对象无独立查询语义，顶层键才可被 LogsQL 过滤与读侧取回）。
			if raw, err = expandAccessFields(raw, e.Fields); err != nil {
				return err
			}
		}
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	endpoint := b.base + BulkIngestPath + "?_stream_fields=" + url.QueryEscape(streamFields)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return fmt.Errorf("victorialogs: build bulk request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("victorialogs: bulk ingest: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		summary := readBodySummary(res.Body, 256)
		return fmt.Errorf("victorialogs: bulk ingest status %s: %s", res.Status, summary)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return nil
}

// bulkRow 是入湖行字段（设计 §2.3；json tag 即 VL 行字段名）。
type bulkRow struct {
	Time    string `json:"_time"`
	Msg     string `json:"_msg"`
	App     string `json:"app"`
	Service string `json:"service"`
	Source  string `json:"source"`
	// Task 是任务日志归因（DT-5；空 = 非任务行——omitempty 保持既有行
	// schema 零变化）。
	Task   string `json:"task,omitempty"`
	Stderr bool   `json:"stderr,omitempty"`
}

// expandAccessFields 把访问行的结构化字段并入入湖行 JSON（顶层键展开；
// 白名单外键静默丢弃——两侧同一词表防任意键扩散进 VL 行 schema）。
func expandAccessFields(raw []byte, fields map[string]string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("victorialogs: expand access fields: %w", err)
	}
	for k, v := range fields {
		if !logs.AllowedAccessFieldKey(k) {
			continue
		}
		obj[k] = v
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("victorialogs: marshal expanded bulk row: %w", err)
	}
	return out, nil
}

// ErrBadQuery 是查询输入非法（白名单/转义前置校验失败）的哨兵——api 层
// 映射 InvalidArgument。
var ErrBadQuery = errors.New("victorialogs: invalid search query")

// BuildLogsQL 构造查询串（构建器的纯函数形态——单测直接钉注入安全）：
// 流过滤 `{app=~"^(…)$",service=~"^(…)$",source=~"^(…)$"}`（值经白名单
// 校验 + QuoteMeta 锚定）+ 关键词短语 `"escaped"`（EscapeLogsQLPhrase）。
// 全空输入返回 `*`（match-all；时间窗由 start/end 参数承载）。
func BuildLogsQL(apps, services, sources, tasks []string, keyword string) (string, error) {
	for _, v := range apps {
		if !appPattern.MatchString(v) {
			return "", fmt.Errorf("%w: app %q not in ^[a-z0-9-]+ or team/prj/app form", ErrBadQuery, v)
		}
	}
	for _, v := range services {
		if !servicePattern.MatchString(v) {
			return "", fmt.Errorf("%w: service %q not in ^[a-z0-9][a-z0-9-]*$", ErrBadQuery, v)
		}
	}
	for _, v := range sources {
		if !allowedSources[v] {
			return "", fmt.Errorf("%w: source %q not in {container, build, access}", ErrBadQuery, v)
		}
	}
	for _, v := range tasks {
		if !taskPattern.MatchString(v) {
			return "", fmt.Errorf("%w: task %q not in ^[0-9A-HJKMNP-TV-Z]{26}$ form", ErrBadQuery, v)
		}
	}
	var parts []string
	if v := streamFilterField("app", apps); v != "" {
		parts = append(parts, v)
	}
	if v := streamFilterField("service", services); v != "" {
		parts = append(parts, v)
	}
	if v := streamFilterField("source", sources); v != "" {
		parts = append(parts, v)
	}
	if v := streamFilterField("task", tasks); v != "" {
		parts = append(parts, v)
	}
	var b strings.Builder
	if len(parts) == 0 {
		// 全空流过滤 → match-all（时间窗由 start/end 参数承载）。
		b.WriteByte('*')
	} else {
		b.WriteByte('{')
		b.WriteString(strings.Join(parts, ","))
		b.WriteByte('}')
	}
	if keyword != "" {
		b.WriteByte(' ')
		b.WriteByte('"')
		b.WriteString(EscapeLogsQLPhrase(keyword))
		b.WriteByte('"')
	}
	return b.String(), nil
}

// streamFilterField 渲染单字段的锚定正则流过滤（空集 = 空 渲染）。
func streamFilterField(field string, values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(values))
	for _, v := range values {
		quoted = append(quoted, regexp.QuoteMeta(v))
	}
	return fmt.Sprintf(`%s=~"^(%s)$"`, field, strings.Join(quoted, "|"))
}

// Search 执行 LogsQL 检索（设计 §3.1：VL 不可达 → 调用方以
// E_LOGS_BACKEND_UNAVAILABLE 诚实报错——本层只回原生错误）。结果为 VL
// 原生序（_time 最大优先——最新命中在前，检索面 UX 口径）。
func (b *Backend) Search(ctx context.Context, q SearchQuery) ([]LogRow, error) {
	query, err := BuildLogsQL(q.Apps, q.Services, q.Sources, q.Tasks, q.Keyword)
	if err != nil {
		return nil, err
	}
	timeout := q.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	vals := url.Values{}
	vals.Set("query", query)
	vals.Set("limit", strconv.Itoa(q.Limit+1)) // 多取 1 行判 has-more
	vals.Set("timeout", timeout.String())
	if q.Offset > 0 {
		vals.Set("offset", strconv.Itoa(q.Offset))
	}
	if !q.Start.IsZero() {
		vals.Set("start", strconv.FormatInt(q.Start.Unix(), 10))
	}
	if !q.End.IsZero() {
		vals.Set("end", strconv.FormatInt(q.End.Unix(), 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+QueryPath,
		strings.NewReader(vals.Encode()))
	if err != nil {
		return nil, fmt.Errorf("victorialogs: build query request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := b.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("victorialogs: query: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		summary := readBodySummary(res.Body, 256)
		return nil, fmt.Errorf("victorialogs: query status %s: %s", res.Status, summary)
	}
	return parseLogRows(res.Body)
}

// parseLogRows 解析 JSON 行流（VL 原生序；坏行跳过——日志面尽力而为，
// 与落盘检索 readDayFile 同口径）。
func parseLogRows(r io.Reader) ([]LogRow, error) {
	var out []LogRow
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue // 坏行跳过
		}
		out = append(out, logRowOf(raw))
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("victorialogs: read query stream: %w", err)
	}
	return out, nil
}

// logRowOf 把 VL 行 JSON 投影为 LogRow（字段缺失容忍——检索面诚实呈现
// 已有维度；_time 解析失败 = 零值行，视图层容忍）。access 行的结构化字段
// 按白名单键回读（入湖与读侧同一词表——logs.AllowedAccessFieldKey）。
func logRowOf(raw map[string]any) LogRow {
	row := LogRow{
		At:      parseVLTime(raw["_time"]),
		App:     rawString(raw["app"]),
		Service: rawString(raw["service"]),
		Source:  rawString(raw["source"]),
		Task:    rawString(raw["task"]),
		Msg:     rawString(raw["_msg"]),
		Stderr:  rawBool(raw["stderr"]),
	}
	for key := range raw {
		if !logs.AllowedAccessFieldKey(key) {
			continue
		}
		if v := rawString(raw[key]); v != "" {
			if row.Fields == nil {
				row.Fields = make(map[string]string)
			}
			row.Fields[key] = v
		}
	}
	return row
}

// parseVLTime 解析 VL 时间值（字符串 = RFC3339 族；数字 = unix 秒~纳秒
// 自动量级；其他 = 零值）。
func parseVLTime(v any) time.Time {
	switch t := v.(type) {
	case string:
		if t == "" || t == "0" {
			return time.Time{}
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
			if ts, err := time.Parse(layout, t); err == nil {
				return ts
			}
		}
		return time.Time{}
	case float64:
		return time.Unix(int64(t), 0)
	default:
		return time.Time{}
	}
}

func rawString(v any) string {
	s, _ := v.(string)
	return s
}

// rawBool 容忍布尔与字符串两形态（ES bulk 入湖的布尔字段以字符串形态
// 回读——实测 stderr 回读为 "false"）。
func rawBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	default:
		return false
	}
}

// readBodySummary 读响应体前 limit 字节做错误摘要（单行化——VL 错误体
// 是单行文本）。
func readBodySummary(r io.Reader, limit int) string {
	raw, _ := io.ReadAll(io.LimitReader(r, int64(limit)))
	summary := strings.TrimSpace(string(raw))
	summary = strings.ReplaceAll(summary, "\n", " ")
	if summary == "" {
		return "(empty body)"
	}
	return summary
}
