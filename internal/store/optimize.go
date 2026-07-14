package store

// chain repack(合成フル統合の実装)。
//
// 長期の世代保持では、デルタチェーンの深さ上限到達後に多数のチャンクが
// 同じ浅い祖先(星の中心)への直接デルタになる「星形」構造ができる
// (rebase の副作用)。星の子は中心からのドリフト(世代差)ぶんデルタが
// 肥大しているため、子同士を「デルタサイズ昇順 ≒ 世代順の近似」で
// 隣接チェーン(パス)に再編成すると、各デルタは隣の世代との小さな差分に
// 置き換えられる。
//
// チャンクはコンテンツアドレス(ハッシュ=無圧縮内容)なので、再編成で
// 変わるのは保存表現(ベース・圧縮方式・ファイル内容)だけであり、
// チャンクの識別子・ファイルマニフェストは一切変わらない。
//
// クラッシュ安全性: 新表現は別名ファイル(rep サフィックス)として先に
// 書き、メタデータの切り替え(bbolt トランザクション)後に旧表現を消す。
// メタデータは常に実在するファイルを指すため、どの時点でクラッシュしても
// 読み出しは一貫する(取り残された新表現ファイルは無害な孤児)。
// 各チャンクは「新表現が現表現より確実に小さい場合のみ」書き換えるため、
// 実行は物理容量を単調減少させる。

import (
	"os"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// starChild は再編成候補(同一ベースを共有するデルタチャンク)。
type starChild struct {
	hash       string
	storedSize int64
}

// OptimizeResult は chain repack の実行結果。
type OptimizeResult struct {
	// StarsScanned は検出した星形(共有ベース)の数。
	StarsScanned int `json:"stars_scanned"`
	// ChunksRepacked は表現を書き換えたチャンク数。
	ChunksRepacked int `json:"chunks_repacked"`
	// BytesBefore / BytesAfter は書き換えたチャンクの保存サイズ合計(前後)。
	BytesBefore int64 `json:"bytes_before"`
	BytesAfter  int64 `json:"bytes_after"`
	// PacksCompacted はコンパクション(copy-forward)で削除したパック数。
	PacksCompacted int `json:"packs_compacted"`
}

// minStarSize はこの数以上の子を持つベースだけを再編成対象にする。
const minStarSize = 4

// Optimize は星形チェーンを検出して再編成し、物理容量を削減する。
// サーバー稼働中に呼んでも安全。同時実行は1つに直列化される。
func (s *Store) Optimize() (*OptimizeResult, error) {
	s.optMu.Lock()
	defer s.optMu.Unlock()

	stars, err := s.collectStars()
	if err != nil {
		return nil, err
	}

	res := &OptimizeResult{StarsScanned: len(stars)}
	for base, children := range stars {
		if err := s.repackStar(base, children, res); err != nil {
			return res, err
		}
	}
	// repack で解放された領域を含め、live 率の低いパックを回収する。
	if err := s.compactPacks(res); err != nil {
		return res, err
	}
	return res, nil
}

// collectStars は「minStarSize 個以上のデルタ子を持つベース」ごとに
// 子の一覧(デルタサイズ昇順)を返す。
func (s *Store) collectStars() (map[string][]starChild, error) {
	stars := make(map[string][]starChild)
	err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if meta.Compression == compressionDelta {
				stars[meta.BaseHash] = append(stars[meta.BaseHash],
					starChild{hash: hash, storedSize: meta.StoredSize})
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	for base, children := range stars {
		if len(children) < minStarSize {
			delete(stars, base)
			continue
		}
		// デルタサイズ昇順 ≒ ベースからのドリフトが小さい順 ≒ 世代順の近似
		sort.Slice(children, func(i, j int) bool {
			return children[i].storedSize < children[j].storedSize
		})
		stars[base] = children
	}
	return stars, nil
}

// repackStar は1つの星形を隣接チェーン(パス)に再編成する。
// prev(直前に処理した子)をベース候補として各子のデルタを取り直し、
// 小さくなる場合のみ置き換える。深さ上限に達したらベースを星の中心に
// 戻して新しいセグメントを始める。
func (s *Store) repackStar(base string, children []starChild, res *OptimizeResult) error {
	baseDepth, ok, err := s.chunkDepth(base)
	if err != nil {
		return err
	}
	if !ok {
		return nil // ベースが消えていたら(並行削除)星ごとスキップ
	}

	prevHash := base
	prevDepth := baseDepth
	for _, child := range children {
		if prevHash == base {
			// セグメント先頭: 中心への直接デルタのままが最善なのでそのまま残し、
			// この子を次の子のベース候補にする。
			prevHash = child.hash
			prevDepth = baseDepth + 1
		} else {
			repacked, err := s.repackChunk(child.hash, prevHash, prevDepth, res)
			if err != nil {
				return err
			}
			if repacked {
				prevHash = child.hash
				prevDepth = prevDepth + 1
			}
			// 改善しなかった子は現表現のまま残し、prev も進めない
			// (次の子は同じ prev と比較する)。
		}
		if prevDepth+1 > s.maxDepth*2 {
			// パスが深くなりすぎたら新セグメント(読み出しコストの上限)。
			prevHash = base
			prevDepth = baseDepth
		}
	}
	return nil
}

// repackChunk は hash の保存表現を「newBase へのデルタ」に取り直し、
// 現表現より小さい場合のみ切り替える。
func (s *Store) repackChunk(hash, newBase string, newBaseDepth int, res *OptimizeResult) (bool, error) {
	// 重い処理(データ読み出し・デルタ圧縮)は bbolt のロック外で行う。
	data, err := s.readChunk(hash)
	if err != nil {
		return false, nil // 並行削除など。スキップ
	}
	baseData, err := s.readChunk(newBase)
	if err != nil {
		return false, nil
	}
	delta, err := s.deltaCompress(baseData, data)
	if err != nil {
		return false, nil
	}

	// 事前チェック: 改善がない・既に同じ表現なら、ファイルを書く前に諦める
	// (rep が現表現と同名の場合に稼働中ファイルを上書きしないための必須条件)。
	newRep := newBase[:8]
	improves := false
	s.db.View(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err == nil && meta != nil &&
			meta.Rep != newRep && int64(len(delta)) < meta.StoredSize {
			improves = true
		}
		return nil
	})
	if !improves {
		return false, nil
	}

	// 新表現を先に書く(小さければパックへ、大きければ別名ファイルへ)。
	loc, err := s.writeRep(hash, newRep, delta)
	if err != nil {
		return false, err
	}

	repacked := false
	var oldPath string
	var orphans []string
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil || meta == nil {
			return err // 並行削除 → スキップ
		}
		// 改善保証を再確認(事前チェックとの間の並行変更に備える)。
		if int64(len(delta)) >= meta.StoredSize || (meta.Rep == newRep && meta.PackID == "") {
			return nil
		}
		newBaseMeta, err := getChunkMeta(tx, newBase)
		if err != nil || newBaseMeta == nil {
			return err // 新ベースが消えていたらスキップ
		}
		// 循環防止: 新ベースの祖先チェーンに自分がいないことを確認。
		for h, i := newBase, 0; h != "" && i < 4*s.maxDepth; i++ {
			if h == hash {
				return nil
			}
			m, err := getChunkMeta(tx, h)
			if err != nil || m == nil {
				break
			}
			h = m.BaseHash
		}

		oldBase := meta.BaseHash
		oldSize := meta.StoredSize

		// 旧表現の解放を先に計上する(meta の場所フィールドを使うため、
		// 新しい場所で上書きする前に行う)。
		oldPath, err = s.releaseRep(tx, hash, meta)
		if err != nil {
			return err
		}

		newBaseMeta.RefCount++
		if err := putChunkMeta(tx, newBase, newBaseMeta); err != nil {
			return err
		}
		meta.Compression = compressionDelta
		meta.BaseHash = newBase
		meta.Depth = newBaseDepth + 1
		meta.StoredSize = int64(len(delta))
		if err := applyRepLocation(tx, meta, newRep, loc, int64(len(delta))); err != nil {
			return err
		}
		if err := putChunkMeta(tx, hash, meta); err != nil {
			return err
		}
		if oldBase != "" {
			orphans, err = s.releaseChunksTx(tx, []string{oldBase})
			if err != nil {
				return err
			}
		}

		res.ChunksRepacked++
		res.BytesBefore += oldSize
		res.BytesAfter += int64(len(delta))
		repacked = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if repacked {
		if oldPath != "" {
			os.Remove(oldPath)
		}
		s.removeChunkFiles(orphans)
	} else {
		// 切り替えなかった場合は新表現を破棄する。
		s.discardNewRep(hash, newRep, loc)
	}
	return repacked, nil
}

func (s *Store) chunkDepth(hash string) (int, bool, error) {
	var depth int
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil {
			return err
		}
		if meta != nil {
			depth, ok = meta.Depth, true
		}
		return nil
	})
	return depth, ok, err
}

func forEachChunkMeta(tx *bolt.Tx, fn func(hash string, meta *ChunkMeta) error) error {
	return tx.Bucket(bucketChunks).ForEach(func(k, v []byte) error {
		var meta ChunkMeta
		if err := unmarshalChunkMeta(v, &meta); err != nil {
			return err
		}
		return fn(string(k), &meta)
	})
}
