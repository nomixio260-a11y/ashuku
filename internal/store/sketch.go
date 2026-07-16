package store

// 類似チャンク検出のための特徴スケッチ(resemblance detection)。
//
// チャンク全体に gear ローリングハッシュを走らせ、サンプリングした
// ハッシュ値に K 個の線形変換をかけた最大値を「特徴(super-feature)」とする
// (DERD / Finesse 系の手法)。内容がわずかに違うだけのチャンクは
// ほとんどのウィンドウ値を共有するため、高確率で特徴が一致する。
// 特徴が1つでも一致したチャンクをデルタ圧縮のベース候補とみなす。

// numTransforms 個の線形変換で特徴を取り、sfGroup 個ずつまとめて
// numSF 個のスーパー特徴(SF)にする(DERD/Odess の標準構成)。
// SF の一致は「グループ内の全特徴が一致」を意味するため、単一特徴の
// any-match より精度が高く、SF が複数あることで再現率も確保される。
const (
	numTransforms = 12
	sfGroup       = 3
	numSF         = numTransforms / sfGroup // 4
)

// sampleMask: 下位7ビットが0の位置だけをサンプリング(平均1/128)。
const sampleMask = 0x7F

// featureMuls / featureAdds は固定シードの splitmix64 で決定的に生成する。
var featureMuls, featureAdds = func() ([numTransforms]uint64, [numTransforms]uint64) {
	var muls, adds [numTransforms]uint64
	s := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		s += 0x9E3779B97F4A7C15
		z := s
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		return z ^ (z >> 31)
	}
	for i := range muls {
		muls[i] = next() | 1 // 乗数は奇数に
		adds[i] = next()
	}
	return muls, adds
}()

// gearTable は固定シードの xorshift64 で決定的に生成する
// (プロセス間・実行間で同一であることが索引の前提)。
var gearTable = func() [256]uint64 {
	var t [256]uint64
	s := uint64(0x243F6A8885A308D3)
	for i := range t {
		s ^= s << 13
		s ^= s >> 7
		s ^= s << 17
		t[i] = s
	}
	return t
}()

// computeFeatures はチャンクのスーパー特徴(SF)を返す。ゼロ値の SF は
// 「特徴なし」として扱われ、索引には登録されない。
func computeFeatures(data []byte) []uint64 {
	var maxes [numTransforms]uint64
	var h uint64
	for _, b := range data {
		h = (h << 1) + gearTable[b]
		if h&sampleMask != 0 {
			continue
		}
		for i := 0; i < numTransforms; i++ {
			v := h*featureMuls[i] + featureAdds[i]
			if v > maxes[i] {
				maxes[i] = v
			}
		}
	}
	// sfGroup 個ずつまとめてスーパー特徴に(FNV-1a 風の混合)
	features := make([]uint64, 0, numSF)
	for g := 0; g < numSF; g++ {
		sf := uint64(0xCBF29CE484222325)
		ok := true
		for j := 0; j < sfGroup; j++ {
			m := maxes[g*sfGroup+j]
			if m == 0 {
				ok = false
				break
			}
			sf = (sf ^ m) * 0x100000001B3
		}
		if ok && sf != 0 {
			features = append(features, sf)
		}
	}
	return features
}
