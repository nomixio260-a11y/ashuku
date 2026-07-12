// Package chunker はコンテンツ定義チャンキング(FastCDC)の薄いラッパー。
// データ内容に基づいて可変長チャンクに分割するため、ファイルの一部が
// 挿入・変更されても後続チャンクの境界がずれず、重複排除が効きやすい。
package chunker

import (
	"errors"
	"io"

	fastcdc "github.com/jotfs/fastcdc-go"
)

const (
	// AverageSize は平均チャンクサイズ(1MiB)。
	AverageSize = 1 << 20
	// MinSize / MaxSize はチャンクサイズの下限・上限。
	MinSize = AverageSize / 4
	MaxSize = AverageSize * 4
)

// Chunk は分割された1チャンク。Data は次の Next 呼び出しまで有効。
type Chunk struct {
	Data []byte
}

// Chunker は io.Reader をチャンク列に分割する。
type Chunker struct {
	cdc *fastcdc.Chunker
}

// New は r を読み取るチャンカーを作る。
func New(r io.Reader) (*Chunker, error) {
	cdc, err := fastcdc.NewChunker(r, fastcdc.Options{
		MinSize:     MinSize,
		AverageSize: AverageSize,
		MaxSize:     MaxSize,
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
