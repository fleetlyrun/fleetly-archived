package substrate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"time"
)

// TestSplitTimestamp Docker timestamps=true 头部解析（RFC3339Nano）。
func TestSplitTimestamp(t *testing.T) {
	at, rest := splitTimestamp("2026-09-18T12:00:00.123456789Z hello world")
	if at.IsZero() {
		t.Fatal("timestamp not parsed")
	}
	if want := time.Date(2026, 9, 18, 12, 0, 0, 123456789, time.UTC); !at.Equal(want) {
		t.Fatalf("at = %v, want %v", at, want)
	}
	if rest != "hello world" {
		t.Fatalf("rest = %q", rest)
	}
	// 无头部（防御形态）原样通过。
	at2, rest2 := splitTimestamp("plain line")
	if !at2.IsZero() || rest2 != "plain line" {
		t.Fatalf("plain: %v %q", at2, rest2)
	}
	// 时间戳后无空格结尾（空内容）。
	at3, rest3 := splitTimestamp("2026-09-18T12:00:00Z ")
	if at3.IsZero() || rest3 != "" {
		t.Fatalf("trailing: %v %q", at3, rest3)
	}
	if strings.Contains(rest3, " ") {
		t.Fatal("rest should be trimmed of header")
	}
}

// muxFrame 拼一帧 stdcopy 多路复用块（8 字节头：流类型字节 + 大端帧长）。
// 生产侧由 docker daemon 编帧，这里手工同构（moby stdcopy 新版未导出
// NewStdWriter，无法用库生成）。
func muxFrame(fd byte, payload []byte) []byte {
	h := make([]byte, 8)
	h[0] = fd
	binary.BigEndian.PutUint32(h[4:8], uint32(len(payload))) //nolint:gosec // G115：测试帧长 = 测试载荷长度，恒在 uint32 值域
	return append(h, payload...)
}

// TestStreamLogsSurvivesHugeLine MG-1 回归（timeout-guard 模式）：>1MiB
// 单行不得终结采集流——scanLines 是 StdCopy 的排水面，读侧提前退出会让
// StdCopy 永久阻塞在无读者的 io.Pipe 写上，streamLogs 永不返回（旧
// bufio.Scanner 实现即此形态）。断言：超长行截断投递（带标记、长度有界、
// 时间戳头部保留）、后续正常行不丢、streamLogs 在流尽后正常返回。
func TestStreamLogsSurvivesHugeLine(t *testing.T) {
	const header = "2026-09-18T12:00:00.000000001Z "
	var src bytes.Buffer
	huge := header + strings.Repeat("x", maxLineLen+64*1024) + "\n"
	src.Write(muxFrame(1, []byte(huge)))
	src.Write(muxFrame(1, []byte("2026-09-18T12:00:00.000000002Z tail-line\n")))

	ctx := context.Background()
	out := make(chan LogLine)
	got := make([]LogLine, 0, 2)
	gotDone := make(chan struct{})
	go func() {
		for l := range out {
			got = append(got, l)
		}
		close(gotDone)
	}()
	streamDone := make(chan struct{})
	go func() {
		streamLogs(ctx, &src, out)
		close(streamDone)
	}()
	select {
	case <-streamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("streamLogs did not return within 10s: oversized line makes the read side exit early and StdCopy block on a pipe write with no reader (MG-1 regression)")
	}
	close(out)
	<-gotDone

	if len(got) != 2 {
		t.Fatalf("lines = %d, want 2 (truncated line + following line must survive)", len(got))
	}
	first := got[0]
	if !strings.HasSuffix(first.Line, truncatedSuffix) {
		t.Fatalf("truncated marker missing (len=%d)", len(first.Line))
	}
	if want := maxLineLen - len(header) + len(truncatedSuffix); len(first.Line) != want {
		t.Fatalf("truncated line len = %d, want %d (prefix capped at %d + marker)", len(first.Line), want, maxLineLen)
	}
	if first.At.IsZero() {
		t.Fatal("timestamp header lost in truncation")
	}
	if got[1].Line != "tail-line" || got[1].At.IsZero() {
		t.Fatalf("line after huge one = %+v, want tail-line with timestamp", got[1])
	}
}

// TestReadLineTruncating 单元：正常行、超限截断（丢弃至换行）、EOF 无换行
// 尾行（截断与不截断两种）。小缓冲（4 字节）强制走 ErrBufferFull 分段路径。
func TestReadLineTruncating(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("aa\nbbbbbbbbbb\ntailx"), 4)
	got, trunc, err := readLineTruncating(br, 4)
	if got != "aa" || trunc || err != nil {
		t.Fatalf("line1 = (%q,%v,%v), want (aa,false,nil)", got, trunc, err)
	}
	got, trunc, err = readLineTruncating(br, 4) // 10 个 b 超 4：截断 + 丢弃至换行
	if got != "bbbb" || !trunc || err != nil {
		t.Fatalf("line2 = (%q,%v,%v), want (bbbb,true,nil)", got, trunc, err)
	}
	got, trunc, err = readLineTruncating(br, 4) // 尾行无换行且超限：截断 + io.EOF
	if got != "tail" || !trunc || err != io.EOF {
		t.Fatalf("line3 = (%q,%v,%v), want (tail,true,io.EOF)", got, trunc, err)
	}
	// 流尽后再读：空内容 + io.EOF（调用方据此终止）。
	got, trunc, err = readLineTruncating(br, 4)
	if got != "" || trunc || err != io.EOF {
		t.Fatalf("drained = (%q,%v,%v), want (\"\",false,io.EOF)", got, trunc, err)
	}
}
