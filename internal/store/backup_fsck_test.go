package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// メタバックアップは有効な bbolt DB を生成し、そこから全ファイルを復元できる。
func TestMetaBackupRestorable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	m := putBytes(t, s, "a", repetitiveData(2<<20))

	bkPath := filepath.Join(dir, "backup.db")
	if _, err := s.BackupMeta(bkPath); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// バックアップを有効な bbolt として開き、マニフェストが読めるか確認
	db, err := bolt.Open(bkPath, 0o600, &bolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("バックアップが有効な bbolt DB ではありません: %v", err)
	}
	defer db.Close()
	found := false
	db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("files"))
		if b == nil {
			return nil
		}
		found = b.Get([]byte(m.ID)) != nil
		return nil
	})
	if !found {
		t.Fatal("バックアップにマニフェストが含まれていません")
	}
}

// ローテーションは最新 keep 個だけ残す。
func TestMetaBackupRotation(t *testing.T) {
	s := newTestStore(t)
	putBytes(t, s, "a", []byte("hello"))

	base := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if _, _, err := s.BackupMetaRotating("", base.Add(time.Duration(i)*time.Second), 3); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(s.dir, "meta-backups"))
	count := 0
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".db" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("ローテーション後のバックアップ数 = %d, want 3", count)
	}
}

// 健全なストアは fsck で不整合なし。
func TestFsckHealthy(t *testing.T) {
	s := newTestStore(t)
	putBytes(t, s, "a", randomData(t, 3<<20))
	putBytes(t, s, "b", repetitiveData(2<<20))
	// デルタ・リージョンも作って複雑な参照状態にする
	putBytes(t, s, "b2", repetitiveData(2<<20))
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}

	res, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Healthy() {
		t.Fatalf("健全なストアで不整合: %+v", res)
	}
}

// 参照カウントを人為的に壊すと fsck が検出し、repair が直す。
func TestFsckRepairsRefcount(t *testing.T) {
	s := newTestStore(t)
	m := putBytes(t, s, "a", randomData(t, 3<<20))

	// 先頭チャンクの RefCount を不正な値に書き換える
	man, _ := s.Manifest(m.ID)
	victim := man.Chunks[0]
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta, _ := getChunkMeta(tx, victim)
		meta.RefCount = 99 // 実際は 1 のはず
		return putChunkMeta(tx, victim, meta)
	})
	if err != nil {
		t.Fatal(err)
	}

	// 検出
	res, _ := s.Fsck(false)
	if res.RefcountMismatches == 0 {
		t.Fatal("参照カウントの不整合が検出されませんでした")
	}
	// 修復
	res, _ = s.Fsck(true)
	if !res.Repaired {
		t.Fatal("repair フラグが立っていません")
	}
	// 修復後は健全
	res, _ = s.Fsck(false)
	if !res.Healthy() {
		t.Fatalf("修復後も不整合: %+v", res)
	}
	// 元データは無事
	if got := getBytes(t, s, m.ID); len(got) != 3<<20 {
		t.Fatal("修復後にデータが壊れました")
	}
}

// 孤児チャンク(参照ゼロだが残存)を fsck repair が回収する。
func TestFsckReclaimsOrphan(t *testing.T) {
	s := newTestStore(t)
	putBytes(t, s, "keep", randomData(t, 2<<20))

	// マニフェストに属さない孤児チャンクを人為的に作る
	orphanData := randomDataSeed(t, 1<<20, 999)
	if err := s.storeChunk(hashOf(orphanData), orphanData, "balanced"); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Stats()

	res, err := s.Fsck(true)
	if err != nil {
		t.Fatal(err)
	}
	if res.OrphanChunks == 0 {
		t.Fatal("孤児チャンクが検出されませんでした")
	}
	after, _ := s.Stats()
	if after.PhysicalBytes >= before.PhysicalBytes {
		t.Fatalf("孤児回収で物理が減っていません: %d → %d", before.PhysicalBytes, after.PhysicalBytes)
	}
	if res2, _ := s.Fsck(false); !res2.Healthy() {
		t.Fatal("孤児回収後も不整合")
	}
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
