// Package chunker はコンテンツ定義チャンキング(FastCDC)の薄いラッパー。
// データ内容に基づいて可変長チャンクに分割するため、ファイルの一部が
// 挿入・変更されても後続チャンクの境界がずれず、重複排除が効きやすい。
package chunker

import (
	"errors"
	"io"

	fastcdc "github.com/jotfs/fastcdc-go"
)

// DefaultAverageSize はデフォルトの平均チャンクサイズ(1MiB)。
// 小さくすると重複排除の粒度が細かくなり dedup 率が上がるが、
// チャンク数(メタデータ量)が増える。
const DefaultAverageSize = 1 << 20

// Chunk は分割された1チャンク。Data は次の Next 呼び出しまで有効。
type Chunk struct {
	Data []byte
}

// Chunker は io.Reader をチャンク列に分割する。
type Chunker struct {
	cdc *fastcdc.Chunker
}

// New は r を平均 avgSize バイトのチャンクに分割するチャンカーを作る。
// 下限は avgSize/4、上限は avgSize*4。
func New(r io.Reader, avgSize int) (*Chunker, error) {
	if avgSize <= 0 {
		avgSize = DefaultAverageSize
	}
	cdc, err := fastcdc.NewChunker(r, fastcdc.Options{
		MinSize:     avgSize / 4,
		AverageSize: avgSize,
		MaxSize:     avgSize * 4,
	})
	if err != nil {
		return nil, err
	}
	return &Chunker{cdc: cdc}, nil
}

// Next は次のチャンクを返す。入力の終端では io.EOF を返す。
func (c *Chunker) Next() (Chunk, error) {
	ch, err := c.cdc.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Chunk{}, io.EOF
		}
		return Chunk{}, err
	}
	return Chunk{Data: ch.Data}, nil
}
