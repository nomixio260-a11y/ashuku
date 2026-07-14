package store

// パックファイル: 小さな保存表現(デルタ等)の集約格納。
//
// デルタ圧縮が効くほど保存表現は小さくなる(数百バイト〜数KB)が、
// 1表現=1ファイルではファイルシステムのブロック粒度(通常4KiB)により
// 実ディスク使用量が見かけの数倍に膨らむ。そこで packThreshold 未満の
// 表現は追記専用のパックファイルに集約し、メタデータに (packID, offset)
// を記録する。
//
// 空き回収: 表現の解放はパックの live バイト数を減らすだけで、
// 実領域は Optimize のコンパクション(live 率の低いパックの生き残りを
// 現行パックへ copy-forward してから削除)が回収する。Data Domain の
// container cleaning と同じ方式。
//
// クラッシュ安全性: パック追記→メタ更新の順で行うため、クラッシュしても
// メタデータが指す領域は常に書き込み済み(取り残された追記は無害なゴミに
// なり、コンパクションで消える)。

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	bolt "go.etcd.io/bbolt"
)

const (
	// packThreshold 未満の表現はパックに集約する。
	packThreshold = 128 << 10
	// maxPackSize を超えたパックは閉じて新しいパックに切り替える。
	maxPackSize = 64 << 20
	// syncEvery バイト追記するごとに fsync する(クラッシュ時の
	// 未書き込み窓を制限しつつ、小さな追記ごとの fsync を避ける)。
	syncEvery = 4 << 20
)

// compactMinSize 以上かつ live 率 50% 未満のパックをコンパクションする
// (小さすぎるパックは copy-forward の価値がない。テストから調整可能)。
var compactMinSize int64 = 4 << 20

// packMeta はパック1本の使用量(bbolt の packs バケットに保存)。
type packMeta struct {
	TotalBytes int64 `json:"total"`
	LiveBytes  int64 `json:"live"`
}

// packWriter は現行パックへの追記を直列化する。
type packWriter struct {
	mu          sync.Mutex
	dir         string
	id          string
	f           *os.File
	off         int64
	sinceSync   int64
}

func newPackWriter(dir string) *packWriter {
	return &packWriter{dir: dir}
}

func (w *packWriter) packPath(id string) string {
	return filepath.Join(w.dir, id+".pack")
}

// append はデータを現行パックに追記し、(packID, offset) を返す。
func (w *packWriter) append(data []byte) (string, int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil || w.off >= maxPackSize {
		if err := w.roll(); err != nil {
			return "", 0, err
		}
	}
	off := w.off
	if _, err := w.f.WriteAt(data, off); err != nil {
		return "", 0, err
	}
	w.off += int64(len(data))
	w.sinceSync += int64(len(data))
	if w.sinceSync >= syncEvery {
		if err := w.f.Sync(); err != nil {
			return "", 0, err
		}
		w.sinceSync = 0
	}
	return w.id, off, nil
}

// roll は現行パックを閉じ、新しいパックを開く。
func (w *packWriter) roll() error {
	if w.f != nil {
		w.f.Sync()
		w.f.Close()
		w.f = nil
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	id := newID()
	f, err := os.OpenFile(w.packPath(id), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w.id, w.f, w.off, w.sinceSync = id, f, 0, 0
	return nil
}

// currentID は現行パックのID(未作成なら空)。
func (w *packWriter) currentID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.id
}

// rollIf は id が現行パックなら閉じて切り替える(コンパクション対象に
// できるようにする)。
func (w *packWriter) rollIf(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.id != id {
		return nil
	}
	return w.roll()
}

func (w *packWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		w.f.Sync()
		w.f.Close()
		w.f = nil
	}
}

// readFromPack はパック内の表現を読み出す。
func (s *Store) readFromPack(packID string, off, length int64) ([]byte, error) {
	f, err := os.Open(s.pw.packPath(packID))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("パック %s の読み出しに失敗: %w", packID[:8], err)
	}
	return buf, nil
}

// adjustPackUsage はパックの使用量カウンタを更新する(tx 内で呼ぶこと)。
func adjustPackUsage(tx *bolt.Tx, packID string, dTotal, dLive int64) error {
	b := tx.Bucket(bucketPacks)
	var pm packMeta
	if raw := b.Get([]byte(packID)); raw != nil {
		if err := unmarshalPackMeta(raw, &pm); err != nil {
			return err
		}
	}
	pm.TotalBytes += dTotal
	pm.LiveBytes += dLive
	return putPackMeta(tx, packID, &pm)
}

// repLocation は保存表現の場所(ファイル or パック)。
type repLocation struct {
	packID  string
	packOff int64
}

// writeRep は保存表現を書き込む。小さければパックへ、大きければ
// 単独ファイル(hash+rep 名)へ。パック使用量の計上は呼び出し側の tx で
// commitRepUsage を呼んで行う。
func (s *Store) writeRep(hash, rep string, data []byte) (repLocation, error) {
	if len(data) < packThreshold {
		packID, off, err := s.pw.append(data)
		if err != nil {
			return repLocation{}, err
		}
		return repLocation{packID: packID, packOff: off}, nil
	}
	return repLocation{}, s.writeChunkFile(hash, rep, data)
}

// applyRepLocation はメタデータに表現の場所を書き込み、パック使用量を計上する。
func applyRepLocation(tx *bolt.Tx, meta *ChunkMeta, rep string, loc repLocation, size int64) error {
	if loc.packID != "" {
		meta.PackID = loc.packID
		meta.PackOff = loc.packOff
		meta.Rep = ""
		return adjustPackUsage(tx, loc.packID, size, size)
	}
	meta.PackID = ""
	meta.PackOff = 0
	meta.Rep = rep
	return nil
}

// releaseRep は表現の解放を計上する(tx 内)。ファイル表現なら削除すべき
// パスを返し、パック表現なら live を減らして空文字を返す。
func (s *Store) releaseRep(tx *bolt.Tx, hash string, meta *ChunkMeta) (string, error) {
	if meta.PackID != "" {
		return "", adjustPackUsage(tx, meta.PackID, 0, -meta.StoredSize)
	}
	return s.chunkPath(hash, meta.Rep), nil
}

// discardNewRep は「書いたが採用しなかった」新表現を破棄する(tx 外)。
// パック追記は取り消せないため、live に計上しないまま放置する
// (コンパクションで自然に回収されるゴミになる)。ただし total は
// 増えていないので live 率をわずかに歪めるだけで害はない。
// ファイル表現は単純に削除する。
func (s *Store) discardNewRep(hash, rep string, loc repLocation) {
	if loc.packID == "" {
		os.Remove(s.chunkPath(hash, rep))
	}
}

// compactPacks は live 率の低いパックの生き残りを現行パックへ移し、
// 古いパックを削除して実ディスクを回収する。Optimize から呼ばれる。
func (s *Store) compactPacks(res *OptimizeResult) error {
	current := s.pw.currentID()

	// 対象パックの選定
	type target struct {
		id   string
		meta packMeta
	}
	var targets []target
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPacks).ForEach(func(k, v []byte) error {
			var pm packMeta
			if err := unmarshalPackMeta(v, &pm); err != nil {
				return err
			}
			if pm.LiveBytes == 0 || (pm.TotalBytes >= compactMinSize && pm.LiveBytes*2 < pm.TotalBytes) {
				targets = append(targets, target{id: string(k), meta: pm})
			}
			return nil
		})
	})
	if err != nil {
		return err
	}

	for _, t := range targets {
		// 現行パックが対象なら先にロールして非現行にする
		// (追記中のパックを copy-forward すると位置がずれるため)。
		if t.id == current {
			if err := s.pw.rollIf(t.id); err != nil {
				return err
			}
		}
	}

	for _, t := range targets {
		if err := s.compactOnePack(t.id, res); err != nil {
			return err
		}
	}

	// バケットに存在しない孤児パックファイル(クラッシュの取り残し)を掃除
	return s.sweepOrphanPacks()
}

// compactOnePack はパック1本の生き残り表現を現行パックへ移して削除する。
func (s *Store) compactOnePack(packID string, res *OptimizeResult) error {
	// 生き残り(このパックを指すメタ)を収集
	var movers []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if meta.PackID == packID {
				movers = append(movers, hash)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}

	// 移動とバケットエントリ削除を1トランザクションで原子的に行う。
	// (移動先への追記はファイルIOなので tx 内で行っても安全)
	err = s.db.Update(func(tx *bolt.Tx) error {
		for _, hash := range movers {
			meta, err := getChunkMeta(tx, hash)
			if err != nil {
				return err
			}
			if meta == nil || meta.PackID != packID {
				continue // 並行して削除/移動された
			}
			data, err := s.readFromPack(packID, meta.PackOff, meta.StoredSize)
			if err != nil {
				return err
			}
			newPack, newOff, err := s.pw.append(data)
			if err != nil {
				return err
			}
			meta.PackID = newPack
			meta.PackOff = newOff
			if err := putChunkMeta(tx, hash, meta); err != nil {
				return err
			}
			if err := adjustPackUsage(tx, newPack, meta.StoredSize, meta.StoredSize); err != nil {
				return err
			}
		}
		res.PacksCompacted++
		return tx.Bucket(bucketPacks).Delete([]byte(packID))
	})
	if err != nil {
		return err
	}
	return os.Remove(s.pw.packPath(packID))
}

// sweepOrphanPacks はバケットに登録がないパックファイルを削除する
// (コンパクション後のクラッシュ等の取り残し)。
func (s *Store) sweepOrphanPacks() error {
	entries, err := os.ReadDir(s.pw.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	known := map[string]bool{s.pw.currentID(): true}
	s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPacks).ForEach(func(k, _ []byte) error {
			known[string(k)] = true
			return nil
		})
	})
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".pack" {
			continue
		}
		id := name[:len(name)-len(".pack")]
		if !known[id] {
			os.Remove(filepath.Join(s.pw.dir, name))
		}
	}
	return nil
}
