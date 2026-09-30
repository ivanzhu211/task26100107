package photobatch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// expectCrash 设置一次性崩溃钩子并执行 fn，要求 fn 在钩子点 panic（模拟进程被终止）。
func expectCrash(t *testing.T, s *Store, point string, fn func()) {
	t.Helper()
	s.crash = func(p string) {
		if p == point {
			panic("crash at " + p)
		}
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected crash at %s, but fn returned", point)
		}
	}()
	fn()
}

// reopen 模拟进程重启：放弃旧句柄，在同一目录上重新 Open 并恢复。
func reopen(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func assertNoTmpFiles(t *testing.T, dir string) {
	t.Helper()
	root := filepath.Join(dir, photosDirName)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(p, ".tmp") {
			t.Errorf("leftover tmp file: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertSubmittedContent(t *testing.T, s *Store, key string, total int) {
	t.Helper()
	photos, err := s.ReadPhotos(key)
	if err != nil {
		t.Fatalf("ReadPhotos(%s): %v", key, err)
	}
	if len(photos) != total {
		t.Fatalf("ReadPhotos(%s) = %d photos, want %d", key, len(photos), total)
	}
	for i, p := range photos {
		orig, thumb := content(key, i+1)
		if !bytes.Equal(p.Original, orig) || !bytes.Equal(p.Thumbnail, thumb) {
			t.Fatalf("%s photo %d not byte-identical", key, i+1)
		}
	}
}

// 照片对仅部分落盘后崩溃，重启重试，最终提交内容逐字节一致且无重复。
func TestCrashDuringReceivePair(t *testing.T) {
	for _, point := range []string{
		"receive.batch-created", // 批次记录已建，文件未写
		"receive.orig-written",  // 仅原图落盘
		"receive.pair-written",  // 照片对落盘但未登记
		"receive.committed",     // 已登记但调用未返回
	} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			s := reopen(t, dir)
			orig, thumb := content("k", 1)
			expectCrash(t, s, point, func() {
				_ = s.Receive(t0, "k", "siteA", "roof", 2, 1, orig, thumb)
			})
			s.Close()

			s = reopen(t, dir)
			assertNoTmpFiles(t, dir)
			// 以原键重试同一照片
			if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, orig, thumb); err != nil {
				t.Fatalf("retry receive: %v", err)
			}
			// 若第一次已生效，不同内容必须被拒绝而非覆盖
			if point == "receive.committed" {
				if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, []byte("X"), thumb); !errors.Is(err, ErrPhotoConflict) {
					t.Fatalf("conflict after effective receive: %v", err)
				}
			}
			orig2, thumb2 := content("k", 2)
			if err := s.Receive(t0, "k", "siteA", "roof", 2, 2, orig2, thumb2); err != nil {
				t.Fatal(err)
			}
			info, err := s.Query(t0, "k")
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Received) != 2 || info.Received[0] != 1 || info.Received[1] != 2 {
				t.Fatalf("received = %v, want [1 2]（无重复记录）", info.Received)
			}
			if err := s.Submit(t0, "k"); err != nil {
				t.Fatal(err)
			}
			assertSubmittedContent(t, s, "k", 2)
			// 磁盘上恰好 2 张照片对，无残留
			if n := countFiles(t, filepath.Join(dir, photosDirName)); n != 4 {
				t.Fatalf("files = %d, want 4", n)
			}
			assertNoTmpFiles(t, dir)
		})
	}
}

// 提交过程中退出后以原键重试。
func TestCrashDuringSubmit(t *testing.T) {
	for _, point := range []string{"submit.before-commit", "submit.committed"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			s := reopen(t, dir)
			receiveAll(t, s, t0, "k", "siteA", "roof", 3)
			expectCrash(t, s, point, func() {
				_ = s.Submit(t0.Add(time.Hour), "k")
			})
			s.Close()

			s = reopen(t, dir)
			// 以原键重试提交：无论上次是否生效，结果一致
			if err := s.Submit(t0.Add(time.Hour), "k"); err != nil {
				t.Fatalf("retry submit: %v", err)
			}
			if err := s.Submit(t0.Add(2*time.Hour), "k"); err != nil {
				t.Fatalf("second retry submit: %v", err)
			}
			info, err := s.Query(t0.Add(2*time.Hour), "k")
			if err != nil {
				t.Fatal(err)
			}
			if info.Status != StatusSubmitted {
				t.Fatalf("status = %v", info.Status)
			}
			assertSubmittedContent(t, s, "k", 3)
			if n := countFiles(t, filepath.Join(dir, photosDirName)); n != 6 {
				t.Fatalf("files = %d, want 6（无重复照片）", n)
			}
		})
	}
}

// 截止前重启继续接收；期限从首次接收起算，重启不延长。
func TestRestartBeforeDeadline(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	orig1, thumb1 := content("k", 1)
	if err := s.Receive(t0, "k", "siteA", "roof", 3, 1, orig1, thumb1); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// 重启后继续接收
	s = reopen(t, dir)
	orig2, thumb2 := content("k", 2)
	if err := s.Receive(t0.Add(5*time.Hour), "k", "siteA", "roof", 3, 2, orig2, thumb2); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = reopen(t, dir)
	orig3, thumb3 := content("k", 3)
	if err := s.Receive(t0.Add(23*time.Hour), "k", "siteA", "roof", 3, 3, orig3, thumb3); err != nil {
		t.Fatal(err)
	}
	// 另一个批次在 t0 首次接收，期限从首次接收起算
	o1, th1 := content("slow", 1)
	if err := s.Receive(t0.Add(23*time.Hour), "slow", "siteA", "roof", 2, 1, o1, th1); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(t0.Add(23*time.Hour), "k"); err != nil {
		t.Fatal(err)
	}
	assertSubmittedContent(t, s, "k", 3)
	s.Close()

	// 重启后到点照样过期，重启与后续接收不延长期限
	s = reopen(t, dir)
	o2, th2 := content("slow", 2)
	if err := s.Receive(t0.Add(23*time.Hour).Add(ttl), "slow", "siteA", "roof", 2, 2, o2, th2); !errors.Is(err, ErrExpired) {
		t.Fatalf("receive at deadline after restart: %v", err)
	}
}

// 清理中断后反复恢复与迟到重放：旧键不复活、应保留内容完整、应释放文件无残留。
func TestCrashDuringCleanup(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	receiveAll(t, s, t0, "old", "siteA", "roof", 2) // 将过期
	receiveAll(t, s, t0, "done", "siteA", "roof", 2)
	if err := s.Submit(t0, "done"); err != nil { // 已提交，不受期限影响
		t.Fatal(err)
	}
	receiveAll(t, s, t0.Add(23*time.Hour), "live", "siteA", "roof", 1) // 未过期
	s.Close()

	now := t0.Add(ttl)
	// 在每个清理关键点崩溃，崩溃后重新恢复再继续，逐点推进
	for _, point := range []string{"recover.dir", "cleanup.recovered", "cleanup.files-removed", "cleanup.rows-removed"} {
		s, err := open(dir, nil)
		if err != nil {
			t.Fatalf("open before %s: %v", point, err)
		}
		expectCrash(t, s, point, func() {
			_ = s.Cleanup(now)
		})
		s.Close()
	}
	// 中断停止后，清理必须能够完成
	s = reopen(t, dir)
	if err := s.Cleanup(now); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(now); err != nil { // 幂等
		t.Fatal(err)
	}

	// 迟到重放：旧键不复活
	orig, thumb := content("old", 1)
	if err := s.Receive(now, "old", "siteA", "roof", 2, 1, orig, thumb); !errors.Is(err, ErrExpired) {
		t.Fatalf("late replay receive: %v", err)
	}
	if err := s.Submit(now, "old"); !errors.Is(err, ErrExpired) {
		t.Fatalf("late replay submit: %v", err)
	}
	info, err := s.Query(now, "old")
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired {
		t.Fatalf("old status = %v", info.Status)
	}
	if _, err := os.Stat(s.batchPath("old")); !os.IsNotExist(err) {
		t.Fatal("expired batch files not fully removed")
	}

	// 应保留内容完整
	assertSubmittedContent(t, s, "done", 2)
	info, _ = s.Query(now, "live")
	if info.Status != StatusReady {
		t.Fatalf("live status = %v", info.Status)
	}
	if err := s.Submit(now, "live"); err != nil {
		t.Fatal(err)
	}
	assertSubmittedContent(t, s, "live", 1)

	// 应释放文件无残留：磁盘上只剩 done(4) + live(2)
	if n := countFiles(t, filepath.Join(dir, photosDirName)); n != 6 {
		t.Fatalf("files = %d, want 6", n)
	}
	assertNoTmpFiles(t, dir)
	s.Close()

	// 反复恢复不改变结果
	for i := 0; i < 3; i++ {
		s = reopen(t, dir)
		later := now.Add(time.Duration(i) * time.Hour)
		if err := s.Cleanup(later); err != nil {
			t.Fatal(err)
		}
		assertSubmittedContent(t, s, "done", 2)
		assertSubmittedContent(t, s, "live", 1)
		info, err := s.Query(later, "old")
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusExpired {
			t.Fatalf("round %d: old status = %v", i, info.Status)
		}
		s.Close()
	}
}

// 恢复过程本身再次中断。
func TestCrashDuringRecovery(t *testing.T) {
	dir := t.TempDir()
	s := reopen(t, dir)
	receiveAll(t, s, t0, "k", "siteA", "roof", 1)
	s.Close()
	// 手工制造崩溃残留：.tmp 暂存文件与孤儿批次目录
	bdir := filepath.Join(dir, photosDirName)
	if err := os.WriteFile(filepath.Join(s.batchPath("k"), "9.orig.tmp"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(bdir, strings.Repeat("ab", 16))
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "1.orig"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 恢复中断两次后，第三次必须完成
	for i := 0; i < 2; i++ {
		crashed := false
		func() {
			defer func() {
				if recover() != nil {
					crashed = true
				}
			}()
			s, err := open(dir, func(p string) {
				if p == "recover.dir" {
					panic("crash during recovery")
				}
			})
			if err == nil {
				s.Close()
			}
		}()
		if !crashed {
			t.Fatalf("round %d: expected recovery crash", i)
		}
	}
	s = reopen(t, dir)
	assertNoTmpFiles(t, dir)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan dir not reclaimed")
	}
	// 已有批次不受影响，可继续提交
	info, err := s.Query(t0, "k")
	if err != nil || info.Status != StatusReady {
		t.Fatalf("info = %+v, err = %v", info, err)
	}
	if err := s.Submit(t0, "k"); err != nil {
		t.Fatal(err)
	}
	assertSubmittedContent(t, s, "k", 1)
}

// 已接收照片在期限内重启后必须仍能继续提交（跨多次重启）。
func TestReceivedSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	for no := 1; no <= 3; no++ {
		s := reopen(t, dir)
		orig, thumb := content("k", no)
		if err := s.Receive(t0.Add(time.Duration(no-1)*time.Hour), "k", "siteA", "roof", 3, no, orig, thumb); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	s := reopen(t, dir)
	info, err := s.Query(t0.Add(3*time.Hour), "k")
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(info.Received) != "[1 2 3]" {
		t.Fatalf("received = %v", info.Received)
	}
	if err := s.Submit(t0.Add(3*time.Hour), "k"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = reopen(t, dir)
	assertSubmittedContent(t, s, "k", 3)
}
