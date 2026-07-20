package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// TestClientManifestDuplicateChunkRefcount は、クライアント補助アップロードで
// 同一ハッシュが複数回現れるマニフェスト(繰り返しブロックの正常な dedup ケース)を
// 確定したあと、そのファイルを削除しても、同じチャンクを共有する別ファイルが
// 依然として読めること(=参照カウントの数え漏れによる早期 GC=データ損失が
// 起きないこと)を確認する回帰テスト。
//
// 修正前: CommitClientManifest は位置ごとに事前スナップショットした meta を ++ する
// ため、[H,H] でも RefCount は +1 にしかならない。一方 releaseChunksTx は出現ごとに
// -1 するので、削除で 0 まで枯れて共有チャンクを物理削除し、F2 が読めなくなる。
// (チャンクキャッシュが Get を隠蔽しうるので、判定は RefCount を直接検査する。)
func TestClientManifestDuplicateChunkRefcount(t *testing.T) {
	s := newTestStore(t)
	block := bytes.Repeat([]byte("shared-block-content "), 64) // 1344 bytes
	sum := sha256.Sum256(block)
	h := hex.EncodeToString(sum[:])

	refcount := func() (int64, bool) {
		var rc int64
		var exists bool
		if err := s.db.View(func(tx *bolt.Tx) error {
			m, err := getChunkMeta(tx, h)
			if err != nil {
				return err
			}
			if m != nil {
				exists, rc = true, m.RefCount
			}
			return nil
		}); err != nil {
			t.Fatalf("view: %v", err)
		}
		return rc, exists
	}

	if err := s.PutChunkVerified(h, block, "none", int64(len(block))); err != nil {
		t.Fatalf("PutChunkVerified: %v", err)
	}
	// F1 は同一チャンクを 2 回参照。RefCount は +2 でなければならない。
	f1, missing, err := s.CommitClientManifest("F1", "", []string{h, h}, 0, 0)
	if err != nil || len(missing) > 0 {
		t.Fatalf("commit F1: err=%v missing=%v", err, missing)
	}
	if rc, ok := refcount(); !ok || rc != 2 {
		t.Fatalf("F1=[H,H] 後の RefCount=%d(存在=%v)、期待 2(重複ハッシュの数え漏れ)", rc, ok)
	}
	// F2 は同一チャンクを共有(1 回参照)。RefCount は 3。
	f2, missing, err := s.CommitClientManifest("F2", "", []string{h}, 0, 0)
	if err != nil || len(missing) > 0 {
		t.Fatalf("commit F2: err=%v missing=%v", err, missing)
	}
	if rc, ok := refcount(); !ok || rc != 3 {
		t.Fatalf("F2 後の RefCount=%d、期待 3", rc)
	}
	// F1 を削除。releaseChunksTx は [H,H] を出現ごとに -2 する。正しく +2 されて
	// いれば 3-2=1 で H は残り、F2 が参照し続ける。数え漏れていれば 0 まで枯れて GC。
	if err := s.Delete(f1.ID); err != nil {
		t.Fatalf("delete F1: %v", err)
	}
	if rc, ok := refcount(); !ok || rc != 1 {
		t.Fatalf("F1 削除後の RefCount=%d(存在=%v)、期待 1 — 共有チャンクが早期 GC された=データ損失", rc, ok)
	}
	// F2 が依然として内容一致で読めること。
	_, r, err := s.Get(f2.ID)
	if err != nil {
		t.Fatalf("get F2 失敗(データ損失): %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read F2: %v", err)
	}
	if !bytes.Equal(got, block) {
		t.Fatalf("F2 の内容が不一致: got %d bytes want %d", len(got), len(block))
	}
}

// TestSweepStagedRespectsRefreshedTTL は、sweepStagedChunks の走査フェーズで
// 掃除対象に選ばれた staged チャンクでも、削除フェーズ直前に HasChunks が TTL を
// 延長(Staged=now)したなら削除されないことを確認する回帰テスト。
//
// 修正前: 削除フェーズは RefCount だけを再確認し Staged を見ないため、延長された
// チャンクを削除してしまい、「存在する」と通知したのにクライアントのコミットが
// ErrChunksMissing で失敗する(HasChunks の TTL 延長が無意味になる)。
func TestSweepStagedRespectsRefreshedTTL(t *testing.T) {
	s := newTestStore(t)
	block := bytes.Repeat([]byte("staged-block "), 32)
	sum := sha256.Sum256(block)
	h := hex.EncodeToString(sum[:])
	if err := s.PutChunkVerified(h, block, "none", int64(len(block))); err != nil {
		t.Fatalf("PutChunkVerified: %v", err)
	}
	// Staged を TTL 超過(古い)に設定して走査フェーズの選定対象にする。
	if err := s.db.Update(func(tx *bolt.Tx) error {
		m, err := getChunkMeta(tx, h)
		if err != nil {
			return err
		}
		m.Staged = time.Now().Add(-2 * StagedTTL).Unix()
		return putChunkMeta(tx, h, m)
	}); err != nil {
		t.Fatalf("age staged chunk: %v", err)
	}
	// 走査と削除の間で HasChunks が TTL を延長する状況を再現。
	sweepBetweenPhases = func() {
		if _, err := s.HasChunks([]string{h}); err != nil {
			t.Errorf("HasChunks: %v", err)
		}
	}
	defer func() { sweepBetweenPhases = nil }()

	var res OptimizeResult
	if err := s.sweepStagedChunks(&res); err != nil {
		t.Fatalf("sweepStagedChunks: %v", err)
	}
	// 延長された H は残っていなければならない。
	var exists bool
	if err := s.db.View(func(tx *bolt.Tx) error {
		m, err := getChunkMeta(tx, h)
		if err != nil {
			return err
		}
		exists = m != nil
		return nil
	}); err != nil {
		t.Fatalf("view: %v", err)
	}
	if !exists {
		t.Fatal("TTL を延長した staged チャンクが掃除された(HasChunks の TTL 延長が無効化)")
	}
}
