package store

import (
	"testing"

	bolt "go.etcd.io/bbolt"
)

// TestRealChainDepthImmuneToStaleDepth は realChainDepth が BaseHash 構造を辿って
// デルタチェーンの実長を数え、陳腐化した Depth フィールドに惑わされないことを
// 確認する。Optimize の rebase は自分の Depth は更新しても子孫の Depth を据え置く
// ため、上限判定に保存 Depth を信じると新規デルタが maxDepth を超えて伸びうる。
func TestRealChainDepthImmuneToStaleDepth(t *testing.T) {
	s := newTestStore(t)
	h := []string{"root", "c1", "c2", "c3", "c4"}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		if err := putChunkMeta(tx, h[0], &ChunkMeta{Compression: compressionRaw}); err != nil {
			return err
		}
		for i := 1; i < len(h); i++ {
			m := &ChunkMeta{Compression: compressionDelta, BaseHash: h[i-1], Depth: i}
			if err := putChunkMeta(tx, h[i], m); err != nil {
				return err
			}
		}
		// c2 の Depth を 0 に陳腐化させる(実長は変わらないはず)。
		m2, err := getChunkMeta(tx, h[2])
		if err != nil {
			return err
		}
		m2.Depth = 0
		return putChunkMeta(tx, h[2], m2)
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		if d := realChainDepth(tx, h[4], 32); d != 4 {
			t.Errorf("realChainDepth(c4)=%d 期待 4(陳腐化 Depth=0 に影響されないこと)", d)
		}
		if d := realChainDepth(tx, h[0], 32); d != 0 {
			t.Errorf("realChainDepth(root)=%d 期待 0", d)
		}
		// limit で早期打ち切りされること。
		if d := realChainDepth(tx, h[4], 2); d != 3 {
			t.Errorf("realChainDepth(c4, limit=2)=%d 期待 3(limit+1 で打ち切り)", d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
