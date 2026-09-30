package photobatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// reopen 模拟进程重启：关闭后在同一目录重新打开。
func reopen(t *testing.T, s *Store, dir string) *Store {
	t.Helper()
	s.Close()
	ns, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { ns.Close() })
	return ns
}

func TestDeadlineAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// T 时刻接收第 1 张，计时开始
	o1, t1 := photo(1)
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 3, 1, o1, t1, base); err != nil {
		t.Fatal(err)
	}

	// 截止前 1 秒重启，继续接收不受影响
	s = reopen(t, s, dir)
	justBefore := base.Add(TTL - time.Second)
	for seq := 2; seq <= 3; seq++ {
		o, th := photo(seq)
		if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 3, seq, o, th, justBefore); err != nil {
			t.Fatalf("receive before deadline seq=%d: %v", seq, err)
		}
	}
	if _, err := s.Submit(ctx, "k1", justBefore); err != nil {
		t.Fatalf("submit before deadline: %v", err)
	}
}

func TestBatchExpiresAtDeadline(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// k1：收齐但没提交，到达期限同样过期
	receiveN(t, s, "k1", "siteA", "roof", 2, base)
	// k2：只收了一张
	o, th := photo(1)
	if err := s.ReceivePhoto(ctx, "k2", "siteA", "roof", 3, 1, o, th, base); err != nil {
		t.Fatal(err)
	}

	// 重启后到达截止时刻
	s = reopen(t, s, dir)
	deadline := base.Add(TTL)

	for _, key := range []string{"k1", "k2"} {
		if err := s.ReceivePhoto(ctx, key, "siteA", "roof", map[string]int{"k1": 2, "k2": 3}[key], 2, o, th, deadline); !errors.Is(err, ErrBatchExpired) {
			t.Fatalf("%s receive at deadline: %v", key, err)
		}
		if _, err := s.Submit(ctx, key, deadline); !errors.Is(err, ErrBatchExpired) {
			t.Fatalf("%s submit at deadline: %v", key, err)
		}
		info, err := s.Query(ctx, key, deadline)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusExpired {
			t.Fatalf("%s status = %q", key, info.Status)
		}
	}
}

func TestSubmittedBatchSurvivesDeadline(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	receiveN(t, s, "k1", "siteA", "roof", 2, base)
	if _, err := s.Submit(ctx, "k1", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	// 跨过截止时间并重启，内容仍完整可读
	s = reopen(t, s, dir)
	past := base.Add(48 * time.Hour)
	info, err := s.Query(ctx, "k1", past)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusSubmitted {
		t.Fatalf("status = %q", info.Status)
	}
	for seq := 1; seq <= 2; seq++ {
		wantO, wantT := photo(seq)
		o, th, err := s.ReadPhoto(ctx, "k1", seq)
		if err != nil {
			t.Fatalf("ReadPhoto seq=%d: %v", seq, err)
		}
		if string(o) != string(wantO) || string(th) != string(wantT) {
			t.Fatalf("seq=%d content changed", seq)
		}
	}
	// 清理不影响已提交批次
	res, err := s.Cleanup(ctx, past)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ExpiredKeys) != 0 {
		t.Fatalf("cleanup expired submitted batch: %v", res.ExpiredKeys)
	}
	if _, _, err := s.ReadPhoto(ctx, "k1", 1); err != nil {
		t.Fatalf("read after cleanup: %v", err)
	}
}

func TestMonotonicClockClamp(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	// 调用方时钟回退不应让期限判定回退
	o, th := photo(1)
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 1, 1, o, th, base.Add(23*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 用更早的时间查询，期限仍从首次接收起算
	info, err := s.Query(ctx, "k1", base)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusReady {
		t.Fatalf("status = %q", info.Status)
	}
	// 期限从首次接收（base+23h）起算 24 小时，不受时钟回退影响
	if _, err := s.Submit(ctx, "k1", base.Add(47*time.Hour)); !errors.Is(err, ErrBatchExpired) {
		t.Fatalf("submit at deadline: %v", err)
	}
}
