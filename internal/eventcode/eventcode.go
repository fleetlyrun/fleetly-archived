// Package eventcode 是 fleetly 平台事件名注册表（代码内唯一真源）。
//
// 契约纪律（架构 §2.8）：事件与错误码同归注册表管理，稳定字符串、永不
// 复用、只新增；文档域清单（release-semantics §2.7、stateful-placement
// §2.8、state-model §2.9/§2.10、architecture §4.3）为定义性说明。
// 命名格式 `namespace.name`（小写 snake_case，如 deployment.rollback_failed）。
//
// fail-fast：重复注册、非法格式在构造期（包初始化）panic。
package eventcode

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// namePattern 校验事件名格式：namespace.name，两段均为小写字母/数字/
// 下划线、以字母开头。
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$`)

// Event 是一个已注册平台事件名。
type Event struct {
	// Name 形如 "deployment.queued"。
	Name string
	// Summary 一句话语义（注册表内说明）。
	Summary string
	// Source 是该事件的文档出处锚：定义该事件的设计文档章节引用（如
	// "release-semantics §2.7"）、实现期票据注记或设计线引用。出处随名
	// 内联在注册表一处（原 eventcode_test 手抄 docEvents 清单已删除——
	// 注册表是唯一真源，出处是事件名的知识的一部分）。
	Source string
}

// Registry 是事件名注册表（只增）。
type Registry struct {
	events map[string]Event
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry { return &Registry{events: make(map[string]Event)} }

// MustRegister 注册一个事件名：格式非法或重复注册时 panic（构造期
// fail-fast）。
func (r *Registry) MustRegister(e Event) {
	if err := r.register(e); err != nil {
		panic(err)
	}
}

func (r *Registry) register(e Event) error {
	if r.events == nil {
		r.events = make(map[string]Event)
	}
	if !namePattern.MatchString(e.Name) {
		return fmt.Errorf("eventcode: invalid event name format %q (want ^[a-z][a-z0-9_]*\\.[a-z][a-z0-9_]*$)", e.Name)
	}
	if _, dup := r.events[e.Name]; dup {
		return fmt.Errorf("eventcode: event name %s already registered (registry is append-only; names are never reused)", e.Name)
	}
	if e.Summary == "" {
		return fmt.Errorf("eventcode: %s missing summary", e.Name)
	}
	r.events[e.Name] = e
	return nil
}

// Get 按事件名查询；未注册返回 false。
func (r *Registry) Get(name string) (Event, bool) {
	e, ok := r.events[name]
	return e, ok
}

// Len 返回已注册事件数。
func (r *Registry) Len() int { return len(r.events) }

// Names 返回全部事件名（字典序）。
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.events))
	for name := range r.events {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// All 返回全部事件（按 Name 字典序）。
func (r *Registry) All() []Event {
	names := r.Names()
	all := make([]Event, 0, len(names))
	for _, name := range names {
		all = append(all, r.events[name])
	}
	return all
}

// defaultRegistry 是包级默认注册表（events.go 的 builtins 在包初始化期
// 注册——坏名使进程启动即 panic）。
var defaultRegistry = newDefaultRegistry()

func newDefaultRegistry() *Registry {
	r := NewRegistry()
	for _, e := range builtins {
		r.MustRegister(e)
	}
	return r
}

// Default 返回包级默认注册表。
func Default() *Registry { return defaultRegistry }

// Get / Len / Names / All 的包级快捷方式（作用于 Default）。
func Get(name string) (Event, bool) { return defaultRegistry.Get(name) }
func Len() int                      { return defaultRegistry.Len() }
func Names() []string               { return defaultRegistry.Names() }
func All() []Event                  { return defaultRegistry.All() }

// Snapshot 返回注册表的规范化文本快照（golden 测试用）：每行
// name \t summary \t source，按 Name 字典序。
func (r *Registry) Snapshot() string {
	var b strings.Builder
	for _, e := range r.All() {
		b.WriteString(e.Name)
		b.WriteByte('\t')
		b.WriteString(e.Summary)
		b.WriteByte('\t')
		b.WriteString(e.Source)
		b.WriteByte('\n')
	}
	return b.String()
}
