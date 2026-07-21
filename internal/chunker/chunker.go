// Package chunker はコンテンツ定義チャンキング(FastCDC)の薄いラッパー。
// データ内容に基づいて可変長チャンクに分割するため、ファイルの一部が
// 挿入・変更されても後続チャンクの境界がずれず、重複排除が効きやすい。
package chunker

import (
	"errors"
	"io"
)

// DefaultAverageSize はデフォルトの平均チャンクサイズ(256KiB)。
// 小さくすると重複排除の粒度が細かくなり dedup 率が上がるが、チャンク数
// (メタデータ量)が増える。実測(多世代バックアップ)で 1MiB→256KiB は物理
// −6.4%、さらに 64KiB で −15% だが 64KiB は チャンク数が約9倍になりメタデータ/
// CPU 負荷が重い(64KiB 未満は per-chunk 文脈喪失で逆に悪化)。256KiB は dedup
// 向上と 3 倍のチャンク数増のバランス点で、しかも smallChunkMax(512KiB)以下
// なのでファイル横断のソリッド・リージョン化も効く。dedup を極大化したい運用は
// Config.AvgChunkSize=64<<10 を明示指定できる(RESEARCH §4.54)。
const DefaultAverageSize = 256 << 10

// Chunk は分割された1チャンク。Data は次の Next 呼び出しまで有効。
type Chunk struct {
	Data []byte
}

// Chunker は io.Reader をチャンク列に分割する。
type Chunker struct {
	cdc *cdc
}

// New は r を平均 avgSize バイトのチャンクに分割するチャンカーを作る。
// 下限は avgSize/4、上限は avgSize*4。gear テーブルは読み取り専用なので
// 複数の Chunker を並行に使ってもデータ競合しない。
func New(r io.Reader, avgSize int) (*Chunker, error) {
	if avgSize <= 0 {
		avgSize = DefaultAverageSize
	}
	if avgSize < 4 {
		return nil, errors.New("avgSize が小さすぎます")
	}
	return &Chunker{cdc: newCDC(r, avgSize)}, nil
}

// Next は次のチャンクを返す。入力の終端では io.EOF を返す。
func (c *Chunker) Next() (Chunk, error) {
	data, err := c.cdc.next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Chunk{}, io.EOF
		}
		return Chunk{}, err
	}
	return Chunk{Data: data}, nil
}
