package photobatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestHelperProcess 是被测试进程以子进程方式启动的辅助入口，
// 用于在故障注入点被直接“杀死”（os.Exit），模拟进程崩溃。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("PHOTOBATCH_HELPER") != "1" {
		return
	}
	dir := os.Getenv("PHOTOBATCH_DIR")
	scenario := os.Getenv("PHOTOBATCH_SCENARIO")
	ctx := context.Background()
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(2)
	}
	switch scenario {
	case "receive_one":
		o, th := photo(1)
		if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, o, th, base); err != nil {
			fmt.Fprintln(os.Stderr, "receive:", err)
			os.Exit(2)
		}
	case "fill_and_submit":
		for seq := 1; seq <= 3; seq++ {
			o, th := photo(seq)
			if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 3, seq, o, th, base); err != nil {
				fmt.Fprintln(os.Stderr, "receive:", err)
				os.Exit(2)
			}
		}
		if _, err := s.Submit(ctx, "k1", base.Add(time.Hour)); err != nil {
			fmt.Fprintln(os.Stderr, "submit:", err)
			os.Exit(2)
		}
	case "cleanup":
		if _, err := s.Cleanup(ctx, base.Add(48*time.Hour)); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", err)
			os.Exit(2)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown scenario", scenario)
		os.Exit(2)
	}
	os.Exit(0)
}

// runHelper 以子进程运行指定场景；crashAt 非空时在对应故障注入点杀死进程。
func runHelper(t *testing.T, dir, scenario, crashAt string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(),
		"PHOTOBATCH_HELPER=1",
		"PHOTOBATCH_DIR="+dir,
		"PHOTOBATCH_SCENARIO="+scenario,
		"PHOTOBATCH_CRASH_AT="+crashAt,
	)
	out, err := cmd.CombinedOutput()
	if crashAt == "" && err != nil {
		t.Fatalf("helper %s failed: %v\n%s", scenario, err, out)
	}
	if crashAt != "" && err == nil {
		t.Fatalf("helper %s should have crashed at %s", scenario, crashAt)
	}
}

// reopenAt 关闭当前存储（非崩溃路径）并在同目录重新打开。
func reopenAt(t *testing.T, s *Store, dir string) *Store {
	t.Helper()
	if s != nil {
		s.Close()
	}
	ns, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return ns
}

// 照片对仅部分落盘后崩溃，重启重试必须一致且无重复。
func TestCrashPartialPhotoPair(t *testing.T) {
	for _, point := range []string{"receive_after_orig", "receive_after_files", "receive_before_commit"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			runHelper(t, dir, "receive_one", point)

			s := reopenAt(t, nil, dir)
			defer s.Close()
			ctx := context.Background()

			// 崩溃的调用从未返回：允许生效或未生效，但查询必须一致
			info, err := s.Query(ctx, "k1", base)
			if point == "receive_before_commit" {
				// 事务未提交，批次可能根本不存在
				if err != nil && !errors.Is(err, ErrBatchNotFound) {
					t.Fatalf("Query: %v", err)
				}
			} else if err != nil && !errors.Is(err, ErrBatchNotFound) {
				t.Fatalf("Query: %v", err)
			}
			if err == nil && len(info.ReceivedSeqs) > 1 {
				t.Fatalf("duplicate photos: %v", info.ReceivedSeqs)
			}

			// 原样重试同一批次
			for seq := 1; seq <= 2; seq++ {
				o, th := photo(seq)
				if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, seq, o, th, base); err != nil {
					t.Fatalf("retry receive seq=%d: %v", seq, err)
				}
			}
			info, err = s.Query(ctx, "k1", base)
			if err != nil {
				t.Fatal(err)
			}
			if len(info.ReceivedSeqs) != 2 {
				t.Fatalf("received = %v", info.ReceivedSeqs)
			}
			if _, err := s.Submit(ctx, "k1", base); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			for seq := 1; seq <= 2; seq++ {
				wantO, wantT := photo(seq)
				o, th, err := s.ReadPhoto(ctx, "k1", seq)
				if err != nil {
					t.Fatalf("ReadPhoto seq=%d: %v", seq, err)
				}
				if !bytes.Equal(o, wantO) || !bytes.Equal(th, wantT) {
					t.Fatalf("seq=%d content mismatch", seq)
				}
			}
			// 磁盘上恰好一组文件，无重复无残留
			assertBatchFiles(t, dir, "k1", 2)
		})
	}
}

// 提交过程中退出后以原键重试：公开内容逐字节一致且无重复。
func TestCrashDuringSubmit(t *testing.T) {
	dir := t.TempDir()
	runHelper(t, dir, "fill_and_submit", "submit_before_commit")

	s := reopenAt(t, nil, dir)
	defer s.Close()
	ctx := context.Background()

	// 崩溃的提交允许生效或未生效
	info, err := s.Query(ctx, "k1", base)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if info.Status != StatusReady && info.Status != StatusSubmitted {
		t.Fatalf("unexpected status %q", info.Status)
	}

	// 以原键重试提交
	res, err := s.Submit(ctx, "k1", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("retry Submit: %v", err)
	}
	// 再次重复提交返回同一结果
	res2, err := s.Submit(ctx, "k1", base.Add(2*time.Hour))
	if err != nil || !res2.SubmittedAt.Equal(res.SubmittedAt) {
		t.Fatalf("resubmit = %+v, %v", res2, err)
	}
	// 整批逐字节一致
	for seq := 1; seq <= 3; seq++ {
		wantO, wantT := photo(seq)
		o, th, err := s.ReadPhoto(ctx, "k1", seq)
		if err != nil {
			t.Fatalf("ReadPhoto seq=%d: %v", seq, err)
		}
		if !bytes.Equal(o, wantO) || !bytes.Equal(th, wantT) {
			t.Fatalf("seq=%d content mismatch", seq)
		}
	}
	assertBatchFiles(t, dir, "k1", 3)
}

// 清理中断后反复恢复和迟到重放：旧键不复活、应保留内容完整、应释放文件无残留。
func TestCleanupCrashRecoveryAndLateReplay(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	s := reopenAt(t, nil, dir)
	// k1：收齐未提交，将过期
	receiveN(t, s, "k1", "siteA", "roof", 2, base)
	// k2：已提交，必须完整保留
	receiveN(t, s, "k2", "siteA", "roof", 2, base)
	if _, err := s.Submit(ctx, "k2", base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// k3：尚未过期，必须保留
	o3, t3 := photo(1)
	if err := s.ReceivePhoto(ctx, "k3", "siteB", "roof", 2, 1, o3, t3, base.Add(36*time.Hour)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// 第一次清理在标记过期后崩溃
	runHelper(t, dir, "cleanup", "cleanup_after_mark")
	// 第二次清理在删除文件中途崩溃
	runHelper(t, dir, "cleanup", "cleanup_after_first_delete")
	// 反复恢复后最终完成
	runHelper(t, dir, "cleanup", "")

	s = reopenAt(t, nil, dir)
	defer s.Close()
	now := base.Add(48 * time.Hour)

	// 旧键不复活：迟到重放被拒绝，批次保持过期
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, []byte("x"), []byte("y"), now); !errors.Is(err, ErrBatchExpired) {
		t.Fatalf("late replay receive: %v", err)
	}
	if _, err := s.Submit(ctx, "k1", now); !errors.Is(err, ErrBatchExpired) {
		t.Fatalf("late replay submit: %v", err)
	}
	info, err := s.Query(ctx, "k1", now)
	if err != nil {
		t.Fatalf("expired key must remain queryable: %v", err)
	}
	if info.Status != StatusExpired {
		t.Fatalf("k1 status = %q", info.Status)
	}

	// 再次清理幂等
	res, err := s.Cleanup(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range res.ExpiredKeys {
		if k != "k1" {
			t.Fatalf("unexpected expired key %q", k)
		}
	}

	// 已提交批次完整可读
	for seq := 1; seq <= 2; seq++ {
		wantO, wantT := photo(seq)
		o, th, err := s.ReadPhoto(ctx, "k2", seq)
		if err != nil || !bytes.Equal(o, wantO) || !bytes.Equal(th, wantT) {
			t.Fatalf("k2 seq=%d: %v", seq, err)
		}
	}
	// 未过期批次可继续提交
	o, th := photo(2)
	if err := s.ReceivePhoto(ctx, "k3", "siteB", "roof", 2, 2, o, th, base.Add(37*time.Hour)); err != nil {
		t.Fatalf("k3 continue: %v", err)
	}
	if _, err := s.Submit(ctx, "k3", base.Add(37*time.Hour)); err != nil {
		t.Fatalf("k3 submit: %v", err)
	}

	// 最终核对：磁盘上只剩 k2、k3 的完整文件
	assertBatchFiles(t, dir, "k2", 2)
	assertBatchFiles(t, dir, "k3", 2)
	entries, err := os.ReadDir(filepath.Join(dir, "photos"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	if len(got) != 2 || !got["k2"] || !got["k3"] {
		t.Fatalf("leftover dirs: %v", got)
	}
	// 临时目录无残留
	tmpEntries, _ := os.ReadDir(filepath.Join(dir, "tmp"))
	if len(tmpEntries) != 0 {
		t.Fatalf("tmp leftovers: %v", tmpEntries)
	}
}

// assertBatchFiles 校验批次目录下恰好有 total 组原图+缩略图。
func assertBatchFiles(t *testing.T, dir, key string, total int) {
	t.Helper()
	bdir := filepath.Join(dir, "photos", key)
	entries, err := os.ReadDir(bdir)
	if err != nil {
		t.Fatalf("readdir %s: %v", bdir, err)
	}
	if len(entries) != 2*total {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("batch %s has %d files, want %d: %v", key, len(entries), 2*total, names)
	}
	for seq := 1; seq <= total; seq++ {
		for _, kind := range []string{"orig", "thumb"} {
			if _, err := os.Stat(photoPath(filepath.Join(dir, "photos"), key, seq, kind)); err != nil {
				t.Fatalf("missing %s/%d.%s: %v", key, seq, kind, err)
			}
		}
	}
}
