package photobatch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func photo(seq int) (orig, thumb []byte) {
	return []byte(fmt.Sprintf("original-%d", seq)), []byte(fmt.Sprintf("thumb-%d", seq))
}

func receiveN(t *testing.T, s *Store, key, site, stage string, total int, now time.Time) {
	t.Helper()
	for seq := 1; seq <= total; seq++ {
		o, th := photo(seq)
		if err := s.ReceivePhoto(context.Background(), key, site, stage, total, seq, o, th, now); err != nil {
			t.Fatalf("ReceivePhoto seq=%d: %v", seq, err)
		}
	}
}

func TestReceiveSubmitReadBack(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	receiveN(t, s, "k1", "siteA", "roof", 3, base)

	info, err := s.Query(ctx, "k1", base)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if info.Status != StatusReady {
		t.Fatalf("status = %q, want ready", info.Status)
	}
	if len(info.ReceivedSeqs) != 3 {
		t.Fatalf("received = %v", info.ReceivedSeqs)
	}
	// 未提交不可读
	if _, _, err := s.ReadPhoto(ctx, "k1", 1); !errors.Is(err, ErrNotSubmitted) {
		t.Fatalf("ReadPhoto before submit: %v", err)
	}

	res, err := s.Submit(ctx, "k1", base.Add(time.Hour))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.Total != 3 {
		t.Fatalf("SubmitResult = %+v", res)
	}
	// 重复提交返回原有结果
	res2, err := s.Submit(ctx, "k1", base.Add(2*time.Hour))
	if err != nil || !res2.SubmittedAt.Equal(res.SubmittedAt) {
		t.Fatalf("resubmit = %+v, %v", res2, err)
	}
	// 逐字节读回
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
}

func TestDuplicateReceiveIdempotent(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	o, th := photo(1)
	for i := 0; i < 3; i++ {
		if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, o, th, base); err != nil {
			t.Fatalf("dup receive %d: %v", i, err)
		}
	}
	info, _ := s.Query(ctx, "k1", base)
	if len(info.ReceivedSeqs) != 1 || info.ReceivedSeqs[0] != 1 {
		t.Fatalf("received = %v", info.ReceivedSeqs)
	}
	// 相同内容重放（已提交后）也视为同一次接收
	receiveN(t, s, "k1", "siteA", "roof", 2, base)
	if _, err := s.Submit(ctx, "k1", base); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, o, th, base); err != nil {
		t.Fatalf("replay after submit: %v", err)
	}
}

func TestConflictingContentRejected(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	o, th := photo(1)
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, o, th, base); err != nil {
		t.Fatal(err)
	}
	// 原图不同
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, []byte("other"), th, base); !errors.Is(err, ErrPhotoConflict) {
		t.Fatalf("orig conflict: %v", err)
	}
	// 缩略图不同
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, o, []byte("other"), base); !errors.Is(err, ErrPhotoConflict) {
		t.Fatalf("thumb conflict: %v", err)
	}
	// 冲突不覆盖原数据
	receiveN(t, s, "k1", "siteA", "roof", 2, base)
	if _, err := s.Submit(ctx, "k1", base); err != nil {
		t.Fatal(err)
	}
	gotO, gotT, err := s.ReadPhoto(ctx, "k1", 1)
	if err != nil || !bytes.Equal(gotO, o) || !bytes.Equal(gotT, th) {
		t.Fatalf("content overwritten: %v", err)
	}
	// 提交后不同内容同样拒绝
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, []byte("x"), []byte("y"), base); !errors.Is(err, ErrPhotoConflict) {
		t.Fatalf("conflict after submit: %v", err)
	}
}

func TestKeyReuseMismatch(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	o, th := photo(1)
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, o, th, base); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		site, stage string
		total       int
	}{
		{"siteB", "roof", 2},
		{"siteA", "wall", 2},
		{"siteA", "roof", 3},
	}
	for i, c := range cases {
		if err := s.ReceivePhoto(ctx, "k1", c.site, c.stage, c.total, 1, o, th, base); !errors.Is(err, ErrBatchMismatch) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

func TestInvalidInput(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	o, th := photo(1)
	bad := []error{
		s.ReceivePhoto(ctx, "", "s", "st", 1, 1, o, th, base),
		s.ReceivePhoto(ctx, "a/b", "s", "st", 1, 1, o, th, base),
		s.ReceivePhoto(ctx, "..", "s", "st", 1, 1, o, th, base),
		s.ReceivePhoto(ctx, "k", "", "st", 1, 1, o, th, base),
		s.ReceivePhoto(ctx, "k", "s", "", 1, 1, o, th, base),
		s.ReceivePhoto(ctx, "k", "s", "st", 0, 1, o, th, base),
		s.ReceivePhoto(ctx, "k", "s", "st", 51, 1, o, th, base),
		s.ReceivePhoto(ctx, "k", "s", "st", 2, 0, o, th, base),
		s.ReceivePhoto(ctx, "k", "s", "st", 2, 3, o, th, base),
		s.ReceivePhoto(ctx, "k", "s", "st", 1, 1, nil, th, base),
		s.ReceivePhoto(ctx, "k", "s", "st", 1, 1, o, nil, base),
	}
	for i, err := range bad {
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

func TestSubmitIncomplete(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	o, th := photo(1)
	if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 2, 1, o, th, base); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(ctx, "k1", base); !errors.Is(err, ErrBatchIncomplete) {
		t.Fatalf("Submit incomplete: %v", err)
	}
	if _, err := s.Submit(ctx, "nope", base); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("Submit missing: %v", err)
	}
	if _, err := s.Query(ctx, "nope", base); !errors.Is(err, ErrBatchNotFound) {
		t.Fatalf("Query missing: %v", err)
	}
}

func TestQueryPartialProgress(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	for _, seq := range []int{2, 1} {
		o, th := photo(seq)
		if err := s.ReceivePhoto(ctx, "k1", "siteA", "roof", 3, seq, o, th, base); err != nil {
			t.Fatal(err)
		}
	}
	info, err := s.Query(ctx, "k1", base)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusReceiving {
		t.Fatalf("status = %q", info.Status)
	}
	if len(info.ReceivedSeqs) != 2 || info.ReceivedSeqs[0] != 1 || info.ReceivedSeqs[1] != 2 {
		t.Fatalf("seqs = %v", info.ReceivedSeqs)
	}
	if info.Site != "siteA" || info.Stage != "roof" || info.Total != 3 {
		t.Fatalf("info = %+v", info)
	}
	wantExpiry := base.Add(TTL)
	if !info.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("expiresAt = %v, want %v", info.ExpiresAt, wantExpiry)
	}
}
