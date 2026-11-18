package relay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestStreamIdleTimeoutEnv 守住「流式空闲超时」的唯一读取口径。
//
// 这条用例原本在 adapters 包（测包内私有 streamIdleTimeout）。函数上移到 relay 后
// 必须跟着搬 —— 留在原处就只能测一个已经不存在的函数，等于把口径变成无覆盖。
func TestStreamIdleTimeoutEnv(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", streamIdleDefault},
		{"90s", 90 * time.Second},
		{"1m30s", 90 * time.Second},
		{"0", streamIdleDefault},
		{"-5s", streamIdleDefault},
		{"abc", streamIdleDefault},
		{"2h", 30 * time.Minute}, // 上限 30 分钟
	}
	for _, c := range cases {
		t.Setenv("LD_STREAM_IDLE_TIMEOUT", c.env)
		if got := StreamIdleTimeout(); got != c.want {
			t.Fatalf("env=%q 应得到 %s，实际 %s", c.env, c.want, got)
		}
	}
}

// TestUpstreamStreamClientHasNoOverallTimeout 守住「流式 client 整体超时必须为 0」。
//
// 一旦有人给流式 client 设上整体超时，任何超过它的合法长回答都会被中途掐断，
// 且症状是「上游明明在正常输出」—— 这类回归靠肉眼 review 很难发现。
func TestUpstreamStreamClientHasNoOverallTimeout(t *testing.T) {
	if got := UpstreamStreamClient().Timeout; got != 0 {
		t.Fatalf("流式 client 的 Timeout 必须为 0（交给空闲看门狗），实际 %s", got)
	}
	if got := UpstreamClient(0).Timeout; got != DefaultAdapterTimeout {
		t.Fatalf("同步 client 的超时缺省应为 %s，实际 %s", DefaultAdapterTimeout, got)
	}
}

// TestIdleGuardCancelsOnSilence 守住看门狗真的会打断静默连接。
func TestIdleGuardCancelsOnSilence(t *testing.T) {
	ctx, g := NewIdleGuard(context.Background(), 30*time.Millisecond)
	defer g.Stop()
	select {
	case <-ctx.Done():
		if !g.Hit() {
			t.Fatal("由空闲超时取消时 Hit() 必须为 true（决定对外文案与归因）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("空闲超过上限后必须取消 ctx")
	}
}

// TestIdleGuardResetKeepsAlive 守住「每帧重置」：持续有数据就不该被打断。
func TestIdleGuardResetKeepsAlive(t *testing.T) {
	ctx, g := NewIdleGuard(context.Background(), 60*time.Millisecond)
	defer g.Stop()
	for i := 0; i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		g.Reset()
	}
	select {
	case <-ctx.Done():
		t.Fatal("持续收到数据时不应被空闲超时打断")
	default:
	}
	if g.Hit() {
		t.Fatal("未触发空闲超时，Hit() 应为 false")
	}
}

// TestReadLineLimited 守住单帧限长与「返回形态与 ReadBytes 一致」。
func TestReadLineLimited(t *testing.T) {
	// 正常多行：含 '\n'，最后一行无换行也照常交出。
	br := bufio.NewReaderSize(strings.NewReader("data: a\ndata: b\nlast"), 4)
	var got []string
	for {
		line, err := ReadLineLimited(br, 64)
		if len(line) > 0 {
			got = append(got, string(line))
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("结束应返回 EOF，实际 %v", err)
			}
			break
		}
	}
	want := []string{"data: a\n", "data: b\n", "last"}
	if len(got) != len(want) {
		t.Fatalf("应读到 %d 行，实际 %d：%q", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 行应为 %q，实际 %q", i, want[i], got[i])
		}
	}

	// 超长单行：必须报 ErrSSELineTooLong，且不会把整行读进内存。
	long := bytes.Repeat([]byte("x"), 4096)
	br2 := bufio.NewReaderSize(bytes.NewReader(append(long, '\n')), 64)
	if _, err := ReadLineLimited(br2, 1024); !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("超过上限应返回 ErrSSELineTooLong，实际 %v", err)
	}
}
