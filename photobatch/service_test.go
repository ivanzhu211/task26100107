package photobatch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)

func openAt(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// content 生成确定性的照片内容。
func content(key string, no int) (orig, thumb []byte) {
	orig = []byte(fmt.Sprintf("ORIGINAL-%s-%d-%s", key, no, string(make([]byte, 64))))
	thumb = []byte(fmt.Sprintf("THUMB-%s-%d", key, no))
	return orig, thumb
}

func receiveAll(t *testing.T, s *Store, now time.Time, key, site, stage string, total int) {
	t.Helper()
	for no := 1; no <= total; no++ {
		orig, thumb := content(key, no)
		if err := s.Receive(now, key, site, stage, total, no, orig, thumb); err != nil {
			t.Fatalf("Receive %d: %v", no, err)
		}
	}
}

func TestHappyPath(t *testing.T) {
	s := openAt(t, t.TempDir())
	receiveAll(t, s, t0, "k1", "siteA", "roof", 3)

	info, err := s.Query(t0, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusReady {
		t.Fatalf("status = %v, want ready", info.Status)
	}
	if len(info.Received) != 3 {
		t.Fatalf("received = %v", info.Received)
	}
	if _, err := s.ReadPhotos("k1"); !errors.Is(err, ErrNotSubmitted) {
		t.Fatalf("ReadPhotos before submit: %v", err)
	}

	if err := s.Submit(t0.Add(time.Hour), "k1"); err != nil {
		t.Fatal(err)
	}
	// 重复提交返回原有结果
	if err := s.Submit(t0.Add(2*time.Hour), "k1"); err != nil {
		t.Fatalf("duplicate submit: %v", err)
	}
	info, _ = s.Query(t0.Add(2*time.Hour), "k1")
	if info.Status != StatusSubmitted || !info.Submitted {
		t.Fatalf("status = %v", info.Status)
	}

	photos, err := s.ReadPhotos("k1")
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 3 {
		t.Fatalf("got %d photos", len(photos))
	}
	for i, p := range photos {
		orig, thumb := content("k1", i+1)
		if !bytes.Equal(p.Original, orig) || !bytes.Equal(p.Thumbnail, thumb) {
			t.Fatalf("photo %d content mismatch", i+1)
		}
	}
}

func TestReceiveIdempotentAndConflict(t *testing.T) {
	s := openAt(t, t.TempDir())
	orig, thumb := content("k", 1)
	if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, orig, thumb); err != nil {
		t.Fatal(err)
	}
	// 完全相同的重试：同一次接收
	if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, orig, thumb); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	// 原图不同：拒绝
	if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, []byte("OTHER"), thumb); !errors.Is(err, ErrPhotoConflict) {
		t.Fatalf("different original: %v", err)
	}
	// 缩略图不同：拒绝
	if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, orig, []byte("OTHER")); !errors.Is(err, ErrPhotoConflict) {
		t.Fatalf("different thumbnail: %v", err)
	}
	// 收齐并提交后，内容仍是首次接收的版本
	orig2, thumb2 := content("k", 2)
	if err := s.Receive(t0, "k", "siteA", "roof", 2, 2, orig2, thumb2); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(t0, "k"); err != nil {
		t.Fatal(err)
	}
	photos, _ := s.ReadPhotos("k")
	if !bytes.Equal(photos[0].Original, orig) {
		t.Fatal("first received content was overwritten")
	}
	// 提交后不可再修改
	if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, orig, thumb); !errors.Is(err, ErrAlreadySubmitted) {
		t.Fatalf("receive after submit: %v", err)
	}
}

func TestKeyConflict(t *testing.T) {
	s := openAt(t, t.TempDir())
	orig, thumb := content("k", 1)
	if err := s.Receive(t0, "k", "siteA", "roof", 2, 1, orig, thumb); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		site, stage string
		total       int
	}{
		{"siteB", "roof", 2},
		{"siteA", "wall", 2},
		{"siteA", "roof", 3},
	} {
		if err := s.Receive(t0, "k", tc.site, tc.stage, tc.total, 1, orig, thumb); !errors.Is(err, ErrKeyConflict) {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestValidation(t *testing.T) {
	s := openAt(t, t.TempDir())
	orig, thumb := content("k", 1)
	cases := []struct {
		total, no int
		orig      []byte
	}{
		{0, 1, orig}, {51, 1, orig}, {2, 0, orig}, {2, 3, orig}, {2, 1, nil},
	}
	for i, c := range cases {
		if err := s.Receive(t0, fmt.Sprintf("bad%d", i), "s", "st", c.total, c.no, c.orig, thumb); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	// 非 UTC 时间
	local := t0.In(time.FixedZone("CST", 8*3600))
	if err := s.Receive(local, "tz", "s", "st", 1, 1, orig, thumb); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("non-UTC: %v", err)
	}
	// 时钟回退
	if err := s.Receive(t0, "k", "s", "st", 1, 1, orig, thumb); err != nil {
		t.Fatal(err)
	}
	if err := s.Receive(t0.Add(-time.Second), "k", "s", "st", 1, 1, orig, thumb); !errors.Is(err, ErrNonMonotonicClock) {
		t.Fatalf("clock rollback: %v", err)
	}
	if _, err := s.Query(t0.Add(-time.Minute), "k"); !errors.Is(err, ErrNonMonotonicClock) {
		t.Fatalf("query clock rollback: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	s := openAt(t, t.TempDir())
	// 收齐但未提交的批次同样过期
	receiveAll(t, s, t0, "full", "s", "st", 2)
	// 只收了一张的批次
	orig, thumb := content("part", 1)
	if err := s.Receive(t0, "part", "s", "st", 2, 1, orig, thumb); err != nil {
		t.Fatal(err)
	}

	deadline := t0.Add(ttl)
	// 截止前一刻仍可用
	if err := s.Receive(deadline.Add(-time.Nanosecond), "part", "s", "st", 2, 1, orig, thumb); err != nil {
		t.Fatalf("just before deadline: %v", err)
	}
	// 到达 24 小时即过期
	for _, key := range []string{"full", "part"} {
		info, err := s.Query(deadline, key)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status != StatusExpired {
			t.Fatalf("%s status = %v", key, info.Status)
		}
		if err := s.Submit(deadline, key); !errors.Is(err, ErrExpired) {
			t.Fatalf("%s submit: %v", key, err)
		}
	}
	o2, t2 := content("part", 2)
	if err := s.Receive(deadline, "part", "s", "st", 2, 2, o2, t2); !errors.Is(err, ErrExpired) {
		t.Fatalf("receive at deadline: %v", err)
	}
}

func TestSubmittedSurvivesDeadline(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	receiveAll(t, s, t0, "k", "s", "st", 2)
	if err := s.Submit(t0.Add(time.Hour), "k"); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(72 * time.Hour)
	info, err := s.Query(later, "k")
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusSubmitted {
		t.Fatalf("status = %v", info.Status)
	}
	if _, err := s.ReadPhotos("k"); err != nil {
		t.Fatal(err)
	}
	// 清理不影响已提交批次
	if err := s.Cleanup(later); err != nil {
		t.Fatal(err)
	}
	photos, err := s.ReadPhotos("k")
	if err != nil {
		t.Fatal(err)
	}
	orig, _ := content("k", 1)
	if !bytes.Equal(photos[0].Original, orig) {
		t.Fatal("submitted content changed after cleanup")
	}
	if n := countFiles(t, filepath.Join(dir, photosDirName)); n != 4 {
		t.Fatalf("files after cleanup = %d, want 4", n)
	}
}

func TestIncompleteSubmit(t *testing.T) {
	s := openAt(t, t.TempDir())
	orig, thumb := content("k", 1)
	if err := s.Receive(t0, "k", "s", "st", 2, 1, orig, thumb); err != nil {
		t.Fatal(err)
	}
	if err := s.Submit(t0, "k"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("submit: %v", err)
	}
	if _, err := s.Query(t0, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("query missing: %v", err)
	}
	if err := s.Submit(t0, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("submit missing: %v", err)
	}
}

func TestCleanupExpired(t *testing.T) {
	dir := t.TempDir()
	s := openAt(t, dir)
	receiveAll(t, s, t0, "old", "s", "st", 2)                   // 将过期
	receiveAll(t, s, t0.Add(23*time.Hour), "new", "s", "st", 1) // 未过期

	now := t0.Add(ttl) // old 恰好到期
	if err := s.Cleanup(now); err != nil {
		t.Fatal(err)
	}
	// 过期批次文件全部消失，状态仍可查询
	info, err := s.Query(now, "old")
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusExpired {
		t.Fatalf("status = %v", info.Status)
	}
	if _, err := s.ReadPhotos("old"); !errors.Is(err, ErrNotSubmitted) {
		t.Fatalf("read expired: %v", err)
	}
	if _, err := os.Stat(s.batchPath("old")); !os.IsNotExist(err) {
		t.Fatalf("expired batch dir still exists: %v", err)
	}
	// 过期键永久不可复用
	orig, thumb := content("old", 1)
	if err := s.Receive(now, "old", "s", "st", 2, 1, orig, thumb); !errors.Is(err, ErrExpired) {
		t.Fatalf("reuse expired key: %v", err)
	}
	// 未过期批次不受影响
	info, _ = s.Query(now, "new")
	if info.Status != StatusReady {
		t.Fatalf("new status = %v", info.Status)
	}
	if err := s.Submit(now, "new"); err != nil {
		t.Fatal(err)
	}
	// 再次清理幂等
	if err := s.Cleanup(now); err != nil {
		t.Fatal(err)
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
