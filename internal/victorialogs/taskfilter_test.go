package victorialogs

// 任务流选择器回归（T 线 DT-5 / IMPL-T2-1）：task 过滤渲染进流选择器，
// 非 ULID 形态拒绝（注入安全硬性条款同源——白名单字符集之外的字面量不
// 可能逃逸）。

import (
	"errors"
	"strings"
	"testing"
)

func TestBuildLogsQLTaskFilter(t *testing.T) {
	const taskID = "01JABCDEFGHJKMNPQRSTVWXYZ0"
	q, err := BuildLogsQL(nil, nil, nil, []string{taskID}, "")
	if err != nil {
		t.Fatalf("BuildLogsQL task filter: %v", err)
	}
	if !strings.Contains(q, `task=~"^(`+taskID+`)$"`) {
		t.Fatalf("query %q does not carry the anchored task stream filter", q)
	}
	// 与 app 选择器可组合（任务面 app 为空；组合形态仍合法）。
	q, err = BuildLogsQL([]string{"demo"}, nil, []string{"container"}, []string{taskID}, "boom")
	if err != nil {
		t.Fatalf("BuildLogsQL combined: %v", err)
	}
	for _, want := range []string{"app=~", "source=~", "task=~", `"boom"`} {
		if !strings.Contains(q, want) {
			t.Fatalf("combined query %q misses %s", q, want)
		}
	}
	// 负路径：非 ULID 形态（注入载荷/大小写形态）拒绝。
	for _, bad := range []string{"1", "task-1", "01jabcdefghjkmnpqrstvwxyz0", `01J"|drop`, taskID[:25] + "-"} {
		if _, err := BuildLogsQL(nil, nil, nil, []string{bad}, ""); !errors.Is(err, ErrBadQuery) {
			t.Errorf("BuildLogsQL task %q err = %v, want ErrBadQuery", bad, err)
		}
	}
}
