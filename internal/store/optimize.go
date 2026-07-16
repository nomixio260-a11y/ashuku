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

	"github.com/nomixio260-a11y/ashuku/internal/zstdc"
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
	// ZombiesFreed は「ゾンビ救出」で解放されたチャンク数。ゾンビとは、
	// ファイルからは削除済みだが子デルタのベースとしてのみ生き残っている
	// チャンク(保持期限削除後にディスクを占有し続ける主因)。
	ZombiesFreed int `json:"zombies_freed"`
	// DeltaUpgraded はオフラインデルタパスでデルタ化されたチャンク数
	// (クライアント直接アップロード分は取り込み時にデルタを試みないため、
	// ここで後追い圧縮される)。
	DeltaUpgraded int `json:"delta_upgraded"`
	// Recompressed はオフライン再圧縮(本家 libzstd level 19)で表現が
	// 縮んだチャンク数。
	Recompressed int `json:"recompressed"`
	// StagedSwept は TTL 超過で掃除された未コミットチャンク数。
	StagedSwept int `json:"staged_swept"`
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
	// ゾンビ救出: 削除済みファイルのチャンクがベース参照だけで生き残って
	// いる場合、子を祖父へ張り替えてチェーンから外す。チェーンは1パスで
	// 1リンクずつ縮むため、進展がなくなるまで繰り返す。
	for i := 0; i <= s.maxDepth*2; i++ {
		freed, err := s.rescueZombies(res)
		if err != nil {
			return res, err
		}
		if freed == 0 {
			break
		}
	}
	// オフラインデルタパス: 取り込み時にデルタ判定をしていないチャンク
	// (クライアント直接アップロード分)を、背景で類似デルタに圧縮し直す。
	if s.delta {
		if err := s.offlineDeltaPass(res); err != nil {
			return res, err
		}
	}
	// TTL を過ぎた未コミット(staged)チャンクを掃除する。
	if err := s.sweepStagedChunks(res); err != nil {
		return res, err
	}
	// repack・救出で解放された領域を含め、live 率の低いパックを回収する。
	if err := s.compactPacks(res); err != nil {
		return res, err
	}
	return res, nil
}

// offlineDeltaPass はデルタ未判定(DeltaTried=false)の非デルタチャンクに
// 対して類似候補とのデルタ圧縮を試みる。取り込み経路の CPU を最小化しつつ
// (サーバーは検証だけ)、圧縮率は背景で回収する、という分担のための機構。
// 判定結果は成否にかかわらず DeltaTried に記録し、再評価しない。
func (s *Store) offlineDeltaPass(res *OptimizeResult) error {
	type cand struct {
		hash     string
		features []uint64
	}
	var todo []cand
	err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if !meta.DeltaTried && meta.Compression != compressionDelta &&
				meta.RefCount > 0 && len(meta.Features) > 0 {
				todo = append(todo, cand{hash: hash, features: append([]uint64(nil), meta.Features...)})
			}
			return nil
		})
	})
	if err != nil {
		return err
	}

	for _, c := range todo {
		// 類似候補を検索(自分自身は除く)
		var base string
		s.db.View(func(tx *bolt.Tx) error {
			for _, h := range lookupSketches(tx, c.features) {
				if h == c.hash {
					continue
				}
				meta, err := getChunkMeta(tx, h)
				if err == nil && meta != nil && meta.Depth < s.maxDepth {
					base = h
					break
				}
			}
			return nil
		})
		deltaDone := false
		if base != "" {
			done, err := s.repackChunk(c.hash, base, res)
			if err != nil {
				return err
			}
			if done {
				res.DeltaUpgraded++
				deltaDone = true
			}
		}
		// デルタ化しなかったチャンクは、本家 libzstd(level 19)での
		// 再圧縮を試す(クライアントの純Goエンコーダより 8〜10% 縮む)。
		if !deltaDone && zstdc.Available() {
			done, err := s.recompressChunk(c.hash, res)
			if err != nil {
				return err
			}
			if done {
				res.Recompressed++
			}
		}
		// 成否にかかわらず判定済みを記録
		err = s.db.Update(func(tx *bolt.Tx) error {
			meta, err := getChunkMeta(tx, c.hash)
			if err != nil || meta == nil {
				return err
			}
			meta.DeltaTried = true
			return putChunkMeta(tx, c.hash, meta)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// recompressChunk はチャンクの保存表現を最強エンコーダで圧縮し直し、
// 現表現より小さい場合のみ切り替える(表現の差し替えは repack と同じ
// クラッシュ安全機構を使う)。
func (s *Store) recompressChunk(hash string, res *OptimizeResult) (bool, error) {
	data, err := s.readChunk(hash)
	if err != nil {
		return false, nil // 並行削除など。スキップ
	}
	out, err := zstdc.Compress(data)
	if err != nil {
		return false, nil
	}

	// 事前チェック(改善なしならファイルを書かない)
	newRep := "z19"
	improves := false
	s.db.View(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err == nil && meta != nil && meta.Compression != compressionDelta &&
			!(meta.Rep == newRep && meta.PackID == "") &&
			int64(len(out)) < meta.StoredSize {
			improves = true
		}
		return nil
	})
	if !improves {
		return false, nil
	}

	loc, err := s.writeRep(hash, newRep, out)
	if err != nil {
		return false, err
	}
	done := false
	var oldPath string
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil || meta == nil {
			return err
		}
		if meta.Compression == compressionDelta ||
			int64(len(out)) >= meta.StoredSize ||
			(meta.Rep == newRep && meta.PackID == "") {
			return nil
		}
		oldSize := meta.StoredSize
		oldPath, err = s.releaseRep(tx, hash, meta)
		if err != nil {
			return err
		}
		meta.Compression = compressionZstd
		meta.StoredSize = int64(len(out))
		if err := applyRepLocation(tx, meta, newRep, loc, int64(len(out))); err != nil {
			return err
		}
		if err := putChunkMeta(tx, hash, meta); err != nil {
			return err
		}
		res.BytesBefore += oldSize
		res.BytesAfter += int64(len(out))
		done = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if done {
		if oldPath != "" {
			os.Remove(oldPath)
		}
	} else {
		s.discardNewRep(hash, newRep, loc)
	}
	return done, nil
}

// rescueZombies は1パスのゾンビ救出を行い、解放できたゾンビ数を返す。
//
// ゾンビ = デルタチャンクのうち、参照カウントの全てが「子デルタからの
// ベース参照」で占められているもの(どのファイルマニフェストからも
// 到達されない…わけではない点に注意: マニフェスト参照はチャンク列に
// 含まれるので RefCount に計上される。子参照数 == RefCount なら
// マニフェスト参照ゼロと判定できる)。
//
// 子をゾンビの親(祖父)に張り替えるとゾンビの参照がゼロになり解放される。
// 子のデルタは世代距離が1つ増えるぶん大きくなりうるので、
// 「子の増分合計 < ゾンビの保存サイズ」の場合のみ実行する(純減の保証)。
func (s *Store) rescueZombies(res *OptimizeResult) (int, error) {
	type zombie struct {
		hash     string
		base     string // 祖父(子の張り替え先)
		stored   int64
		children []string
	}

	// スナップショット収集: 子参照数と候補ゾンビ
	childrenOf := make(map[string][]string)
	metas := make(map[string]*ChunkMeta)
	err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			m := *meta
			metas[hash] = &m
			if meta.Compression == compressionDelta {
				childrenOf[meta.BaseHash] = append(childrenOf[meta.BaseHash], hash)
			}
			return nil
		})
	})
	if err != nil {
		return 0, err
	}

	var zombies []zombie
	for hash, meta := range metas {
		kids := childrenOf[hash]
		if meta.Compression != compressionDelta || len(kids) == 0 {
			continue // 救出対象はデルタゾンビのみ(plain アンカーは残す)
		}
		if meta.RefCount != int64(len(kids)) {
			continue // マニフェストから参照されている(生きている)
		}
		zombies = append(zombies, zombie{
			hash: hash, base: meta.BaseHash, stored: meta.StoredSize, children: kids,
		})
	}

	freed := 0
	for _, z := range zombies {
		// 祖父の現況確認(このパス中の先行救出で消えている可能性がある)
		baseMeta := metas[z.base]
		if baseMeta == nil {
			continue
		}
		// 子ごとの張り替えデルタを試作し、純減になる場合のみ適用する。
		type plan struct {
			hash  string
			delta []byte
		}
		var plans []plan
		var oldSum, newSum int64
		ok := true
		for _, child := range z.children {
			cm := metas[child]
			if cm == nil || cm.BaseHash != z.hash {
				ok = false // 状況が変わった(別パスで張り替え済み等)
				break
			}
			data, err := s.readChunk(child)
			if err != nil {
				ok = false
				break
			}
			baseData, err := s.readChunk(z.base)
			if err != nil {
				ok = false
				break
			}
			delta, err := s.deltaCompress(baseData, data)
			if err != nil {
				ok = false
				break
			}
			oldSum += cm.StoredSize
			newSum += int64(len(delta))
			plans = append(plans, plan{hash: child, delta: delta})
		}
		if !ok || newSum-oldSum >= z.stored {
			continue // 純減にならないので現状維持
		}
		applied := 0
		for _, p := range plans {
			done, err := s.applyRebase(p.hash, z.base, p.delta, res, false)
			if err != nil {
				return freed, err
			}
			if done {
				applied++
			}
		}
		if applied == len(z.children) {
			freed++
			res.ZombiesFreed++
		}
	}
	return freed, nil
}

// applyRebase は hash の保存表現を「newBase へのデルタ」に張り替える。
// requireImprovement が真なら「新表現が現表現より小さい」場合のみ適用する
// (chain repack 用)。偽なら サイズ増も許容する(ゾンビ救出用 —
// 呼び出し側がゾンビ解放との純減判定を済ませている)。
func (s *Store) applyRebase(hash, newBase string, delta []byte, res *OptimizeResult, requireImprovement bool) (bool, error) {
	newRep := newBase[:8]
	loc, err := s.writeRep(hash, newRep, delta)
	if err != nil {
		return false, err
	}

	done := false
	var oldPath string
	var orphans []string
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil || meta == nil {
			return err
		}
		if requireImprovement && int64(len(delta)) >= meta.StoredSize {
			return nil // 改善保証を再確認(並行変更に備える)
		}
		if meta.Rep == newRep && meta.PackID == "" {
			return nil // 既に同名ファイル表現 → 上書き危険なのでスキップ
		}
		newBaseMeta, err := getChunkMeta(tx, newBase)
		if err != nil || newBaseMeta == nil {
			return err
		}
		// 循環防止
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
		meta.Depth = newBaseMeta.Depth + 1
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
		done = true
		return nil
	})
	if err != nil {
		return false, err
	}
	if done {
		if oldPath != "" {
			os.Remove(oldPath)
		}
		s.removeChunkFiles(orphans)
	} else {
		s.discardNewRep(hash, newRep, loc)
	}
	return done, nil
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
			repacked, err := s.repackChunk(child.hash, prevHash, res)
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
func (s *Store) repackChunk(hash, newBase string, res *OptimizeResult) (bool, error) {
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

	// 適用は applyRebase に委譲(改善保証つき)。
	return s.applyRebase(hash, newBase, delta, res, true)
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
