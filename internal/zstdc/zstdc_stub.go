//go:build !cgo

// CGO 無効ビルド用のスタブ。純Goエンコーダ(klauspost)にフォールバックする。
package zstdc

import "errors"

// Available はこのビルドで本家 libzstd が使えるかを返す。
func Available() bool { return false }

// Level は本家エンコーダで使う圧縮レベル(参考値)。
const Level = 19

// Compress は常にエラーを返す(CGO 無効)。
func Compress(data []byte) ([]byte, error) {
	return nil, errors.New("このビルドは libzstd 非対応です(CGO 無効)")
}
