// Package errcode 是 fleetly 错误码注册表（代码内唯一真源）。
//
// 契约纪律（架构 §2.8「契约版本化纪律」）：错误码为稳定字符串
// （`E_<域>_<条件>` / `W_<域>_<条件>`），永不复用、只新增；文档域清单
// （release-semantics §2.7、stateful-placement §2.8/§2.9、state-model
// §2.7/§2.9、architecture §2.3/§2.4）为定义性说明，注册表为运行时真源。
// 每码携带默认 HTTP 状态映射、默认 suggestion 文案与 docs 锚点。
//
// fail-fast：重复注册、非法格式、E_/W_ 与 HTTP 映射不一致均在构造期
// （Default 注册表构建 = 包初始化）panic，保证「只增、不复用」在进程
// 启动即被校验。
package errcode

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DocsURLPrefix 是错误码文档锚点 URL 前缀（占位：文档站域名与路径结构
// 待 T0.5 契约冻结定稿；锚点 = 前缀 + 码 ID）。
const DocsURLPrefix = "https://docs.fleetly.dev/errors/"

// codePattern 校验稳定码格式：E_ / W_ 前缀 + 大写字母/数字/下划线段
// （拒绝小写、空段、缺前缀）。
var codePattern = regexp.MustCompile(`^[EW]_[A-Z0-9]+(_[A-Z0-9]+)*$`)

// Code 是一个已注册错误码。
type Code struct {
	// ID 稳定码字符串，如 "E_COMPOSE_UNSUPPORTED"。
	ID string
	// HTTP 是该码的默认 HTTP 状态映射；W_ 警告码恒为 0（警告是资源上的
	// 标注/事件，不作为 HTTP 错误出现，发布专项 §2.7「部署失败是资源终态
	// 而非 HTTP 错误」同理适用）。
	HTTP int
	// Summary 一句话语义（注册表内说明，不进信封）。
	Summary string
	// Suggestion 默认修复建议（ErrorResponse.suggestion 的默认占位文案，
	// 业务侧可覆盖；措辞冻结前可调整）。
	Suggestion string
	// Source 是该码的文档出处锚：定义该码的设计文档章节引用（如
	// "release-semantics §2.7"）、实现期票据注记或设计线引用。出处随码
	// 内联在注册表一处（原 errcode_test 手抄 docCodes 清单已删除——注册表
	// 是唯一真源，出处是码的知识的一部分）。
	Source string
}

// Docs 返回该码的文档锚点 URL（DocsURLPrefix + ID）。
func (c Code) Docs() string { return DocsURLPrefix + c.ID }

// Registry 是错误码注册表（只增：注册成功后不可删除或改写）。
type Registry struct {
	codes map[string]Code
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry { return &Registry{codes: make(map[string]Code)} }

// MustRegister 注册一个码：格式非法、重复注册、HTTP 映射与码类不一致时
// panic（构造期 fail-fast）。
func (r *Registry) MustRegister(c Code) {
	if err := r.register(c); err != nil {
		panic(err)
	}
}

func (r *Registry) register(c Code) error {
	if r.codes == nil {
		r.codes = make(map[string]Code)
	}
	if !codePattern.MatchString(c.ID) {
		return fmt.Errorf("errcode: invalid code format %q (want ^[EW]_[A-Z0-9]+(_[A-Z0-9]+)*$; lowercase/missing prefix/empty segment rejected)", c.ID)
	}
	if _, dup := r.codes[c.ID]; dup {
		return fmt.Errorf("errcode: code %s already registered (registry is append-only; codes are never reused)", c.ID)
	}
	isWarning := strings.HasPrefix(c.ID, "W_")
	if isWarning && c.HTTP != 0 {
		return fmt.Errorf("errcode: warning code %s must not carry a default HTTP status (got %d; warnings are never returned as HTTP errors)", c.ID, c.HTTP)
	}
	if !isWarning && (c.HTTP < 400 || c.HTTP > 599) {
		return fmt.Errorf("errcode: error code %s default HTTP status must be 4xx/5xx (got %d)", c.ID, c.HTTP)
	}
	if c.Suggestion == "" {
		return fmt.Errorf("errcode: %s missing default suggestion text", c.ID)
	}
	if c.Summary == "" {
		return fmt.Errorf("errcode: %s missing summary", c.ID)
	}
	r.codes[c.ID] = c
	return nil
}

// Get 按码 ID 查询；未注册返回 false。
func (r *Registry) Get(id string) (Code, bool) {
	c, ok := r.codes[id]
	return c, ok
}

// Len 返回已注册码数。
func (r *Registry) Len() int { return len(r.codes) }

// IDs 返回全部码 ID（字典序）。
func (r *Registry) IDs() []string {
	ids := make([]string, 0, len(r.codes))
	for id := range r.codes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// All 返回全部码（按 ID 字典序）。
func (r *Registry) All() []Code {
	ids := r.IDs()
	all := make([]Code, 0, len(ids))
	for _, id := range ids {
		all = append(all, r.codes[id])
	}
	return all
}

// defaultRegistry 是包级默认注册表（codes.go 的 builtins 在包初始化期
// 注册——坏码使进程启动即 panic）。
var defaultRegistry = newDefaultRegistry()

func newDefaultRegistry() *Registry {
	r := NewRegistry()
	for _, c := range builtins {
		r.MustRegister(c)
	}
	return r
}

// Default 返回包级默认注册表。
func Default() *Registry { return defaultRegistry }

// Get / Len / IDs / All 的包级快捷方式（作用于 Default）。
func Get(id string) (Code, bool) { return defaultRegistry.Get(id) }
func Len() int                   { return defaultRegistry.Len() }
func IDs() []string              { return defaultRegistry.IDs() }
func All() []Code                { return defaultRegistry.All() }

// HTTPStatus 返回码的默认 HTTP 状态；未注册码返回 0（调用方须自行兜底，
// 注册表不发明文档外码）。
func HTTPStatus(id string) int {
	c, ok := defaultRegistry.Get(id)
	if !ok {
		return 0
	}
	return c.HTTP
}

// Snapshot 返回注册表的规范化文本快照（golden 测试用）：每行
// ID \t HTTP \t docs \t summary \t suggestion \t source，按 ID 字典序。
func (r *Registry) Snapshot() string {
	var b strings.Builder
	for _, c := range r.All() {
		b.WriteString(c.ID)
		b.WriteByte('\t')
		b.WriteString(strconv.Itoa(c.HTTP))
		b.WriteByte('\t')
		b.WriteString(c.Docs())
		b.WriteByte('\t')
		b.WriteString(c.Summary)
		b.WriteByte('\t')
		b.WriteString(c.Suggestion)
		b.WriteByte('\t')
		b.WriteString(c.Source)
		b.WriteByte('\n')
	}
	return b.String()
}
