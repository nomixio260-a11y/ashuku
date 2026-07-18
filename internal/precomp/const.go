package precomp

// precompression 共通の定数(cgo の有無にかかわらず必要)。
// JPEG 経路は純 Go(cgo 不要)なので、これらは無タグのこのファイルで定義し、
// CGO 無効ビルドでも参照できるようにする。

const (
	// minStreamSize 未満の入力は分解を試みない(誤検出対策)。
	minStreamSize = 64
	// maxMembers を超えるマルチメンバー/コンテナは分解しない
	// (BGZF のような巨大ファイルで探索が暴走しないように)。
	maxMembers = 1024
	// maxPlainTotal を超える展開は打ち切る(zip bomb 対策・メモリ上限)。
	maxPlainTotal = 1 << 30
)
