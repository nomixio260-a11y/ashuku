package store

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"testing"
)

// 開発用実験: 多数の小ファイルの圧縮率(ASHUKU_SMALL_EXP=1 で実行)
func TestSmallFilesRatioExp(t *testing.T) {
	t.Parallel()
	if os.Getenv("ASHUKU_SMALL_EXP") == "" {
		t.Skip("ASHUKU_SMALL_EXP 未設定")
	}
	s := newTestStore(t)
	rng := rand.New(rand.NewSource(21))
	names := []string{"tanaka", "suzuki", "sato", "kobayashi", "watanabe"}
	var logical int64
	for i := 0; i < 2000; i++ {
		var b bytes.Buffer
		fmt.Fprintf(&b, `{"order_id":"ORD-%08d","customer":{"name":"%s","email":"user%d@example.com","tier":"%s"},"items":[`,
			rng.Intn(1<<26), names[rng.Intn(5)], rng.Intn(99999), []string{"free", "pro", "enterprise"}[rng.Intn(3)])
		n := 3 + rng.Intn(15)
		for j := 0; j < n; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"sku":"SKU-%05d","qty":%d,"price":%d.%02d,"warehouse":"%s"}`,
				rng.Intn(9999), 1+rng.Intn(9), rng.Intn(9999), rng.Intn(100), []string{"tokyo", "osaka", "fukuoka"}[rng.Intn(3)])
		}
		fmt.Fprintf(&b, `],"status":"%s","created_at":"2026-07-%02dT%02d:%02d:%02dZ"}`,
			[]string{"pending", "shipped", "delivered"}[rng.Intn(3)], 1+rng.Intn(28), rng.Intn(24), rng.Intn(60), rng.Intn(60))
		logical += int64(b.Len())
		if _, err := s.Put(fmt.Sprintf("order-%04d.json", i), bytes.NewReader(b.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Stats()
	t.Logf("取り込み直後: logical=%d physical=%d ratio=%.2fx", st.LogicalBytes, st.PhysicalBytes, float64(st.LogicalBytes)/float64(st.PhysicalBytes))
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Stats()
	t.Logf("Optimize 後:   logical=%d physical=%d ratio=%.2fx (region=%d)", st.LogicalBytes, st.PhysicalBytes, float64(st.LogicalBytes)/float64(st.PhysicalBytes), st.RegionCount)
	// 参照: 全ファイル連結を brotli-11 で1本にした場合(理論上限に近い)
}

// 理論上限の参考値: 全ファイル連結を brotli-11 で1本に圧縮
func TestSmallFilesUpperBound(t *testing.T) {
	t.Parallel()
	if os.Getenv("ASHUKU_SMALL_EXP") == "" {
		t.Skip("ASHUKU_SMALL_EXP 未設定")
	}
	rng := rand.New(rand.NewSource(21))
	names := []string{"tanaka", "suzuki", "sato", "kobayashi", "watanabe"}
	var all bytes.Buffer
	for i := 0; i < 2000; i++ {
		var b bytes.Buffer
		fmt.Fprintf(&b, `{"order_id":"ORD-%08d","customer":{"name":"%s","email":"user%d@example.com","tier":"%s"},"items":[`,
			rng.Intn(1<<26), names[rng.Intn(5)], rng.Intn(99999), []string{"free", "pro", "enterprise"}[rng.Intn(3)])
		n := 3 + rng.Intn(15)
		for j := 0; j < n; j++ {
			if j > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"sku":"SKU-%05d","qty":%d,"price":%d.%02d,"warehouse":"%s"}`,
				rng.Intn(9999), 1+rng.Intn(9), rng.Intn(9999), rng.Intn(100), []string{"tokyo", "osaka", "fukuoka"}[rng.Intn(3)])
		}
		fmt.Fprintf(&b, `],"status":"%s","created_at":"2026-07-%02dT%02d:%02d:%02dZ"}`,
			[]string{"pending", "shipped", "delivered"}[rng.Intn(3)], 1+rng.Intn(28), rng.Intn(24), rng.Intn(60), rng.Intn(60))
		all.Write(b.Bytes())
	}
	br := brotliCompressMax(all.Bytes())
	t.Logf("連結+brotli11: %d -> %d (%.2fx)", all.Len(), len(br), float64(all.Len())/float64(len(br)))
}
