package store

// 類似チャンク検出のための特徴スケッチ(resemblance detection)。
//
// チャンク全体に gear ローリングハッシュを走らせ、サンプリングした
// ハッシュ値に K 個の線形変換をかけた最大値を「特徴(super-feature)」とする
// (DERD / Finesse 系の手法)。内容がわずかに違うだけのチャンクは
// ほとんどのウィンドウ値を共有するため、高確率で特徴が一致する。
// 特徴が1つでも一致したチャンクをデルタ圧縮のベース候補とみなす。

const numFeatures = 3

// sampleMask: 下位7ビットが0の位置だけをサンプリング(平均1/128)。
const sampleMask = 0x7F

var featureMuls = [numFeatures]uint64{
	0x9E3779B97F4A7C15, 0xC2B2AE3D27D4EB4F, 0x165667B19E3779F9,
}
var featureAdds = [numFeatures]uint64{
	0x8AE8B026AA35BC85, 0x2545F4914F6CDD1D, 0x27D4EB2F165667C5,
}

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

// computeFeatures はチャンクの特徴値を返す。ゼロ値の特徴は「特徴なし」
// として扱われ、索引には登録されない。
func computeFeatures(data []byte) []uint64 {
	var maxes [numFeatures]uint64
	var h uint64
	for _, b := range data {
		h = (h << 1) + gearTable[b]
		if h&sampleMask != 0 {
			continue
		}
		for i := 0; i < numFeatures; i++ {
			v := h*featureMuls[i] + featureAdds[i]
			if v > maxes[i] {
				maxes[i] = v
			}
		}
	}
	features := make([]uint64, 0, numFeatures)
	for _, m := range maxes {
		if m != 0 {
			features = append(features, m)
		}
	}
	return features
}
