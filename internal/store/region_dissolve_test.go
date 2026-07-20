package store

import (
	"crypto/sha256"
	"os"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// TestDissolveRegionPreservesDataOnReadError は、リージョン解体中にメンバーの
// 読み出しが(一過性に)失敗しても、リージョンファイルが削除されず全データが
// 保持されることを確認する回帰テスト。
//
// 修正前: dissolveRegion は読み出し失敗を continue で握り潰し、末尾で
// リージョンファイルを無条件削除していた。失敗メンバーは RegionID のまま残るので、
// バイトが健全でもリージョンファイル消失で恒久的に読めなくなる(=データ損失)。
func TestDissolveRegionPreservesDataOnReadError(t *testing.T) {
	if testing.Short() {
		t.Skip("リージョン構築を伴う E2E テストのため -short では省略")
	}
	s, err := Open(t.TempDir(), Config{AvgChunkSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	corpus := regionCorpus(t, 1<<20) // 小チャンクで多数の独立チャンク→リージョン化
	m := putBytes(t, s, "corpus", corpus)
	res, err := s.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	if res.RegionsBuilt == 0 {
		t.Skip("この環境ではリージョンが作られなかった(閾値依存)")
	}

	// リージョン ID と、そのメンバーを 1 つ拾う。
	var regionID, victim string
	if err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if meta.RegionID != "" && regionID == "" {
				regionID = meta.RegionID
			}
			if meta.RegionID == regionID && regionID != "" && victim == "" {
				victim = hash
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if regionID == "" || victim == "" {
		t.Skip("リージョンメンバーが見つからなかった")
	}

	// victim メンバーの読み出しを一過性に失敗させる。
	dissolveReadFailHook = func(hash string) error {
		if hash == victim {
			return os.ErrDeadlineExceeded // 一過性エラーの模擬
		}
		return nil
	}
	defer func() { dissolveReadFailHook = nil }()

	// victim がまだリージョンを参照している(=未変換)状態かを確認しておく。
	stillMember := chunkRegionID(t, s, victim) == regionID

	if err := s.dissolveRegion(regionID, res); err != nil {
		t.Fatalf("dissolveRegion: %v", err)
	}

	// 直接の保証(キャッシュに影響されない): victim が未変換のままなら、その唯一の
	// 保管先であるリージョンファイルは削除されていてはならない。修正前は継続して
	// 末尾で無条件削除され、victim が恒久的に読めなくなる(=データ損失)。
	if stillMember && chunkRegionID(t, s, victim) == regionID {
		if _, err := os.Stat(s.regionPath(regionID)); err != nil {
			t.Fatalf("victim が未変換(RegionID=%s)なのにリージョンファイルが削除された"+
				"(=データ損失): %v", regionID, err)
		}
	}

	// 参考: フック解除後は全データが復元できること(キャッシュ経由でも整合)。
	dissolveReadFailHook = nil
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(corpus) {
		t.Fatal("解体後にデータが復元できない")
	}
}

func chunkRegionID(t *testing.T, s *Store, hash string) string {
	t.Helper()
	var id string
	if err := s.db.View(func(tx *bolt.Tx) error {
		m, err := getChunkMeta(tx, hash)
		if err != nil {
			return err
		}
		if m != nil {
			id = m.RegionID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
