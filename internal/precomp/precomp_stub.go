//go:build !cgo

// CGO 無効ビルド用のスタブ。precompression は無効になるが、
// その他の全機能は影響を受けない。
package precomp

import "errors"

// Supported はこのビルドで precompression が使えるかを返す。
func Supported() bool { return false }

// Unwrapped は gzip を分解した結果。
type Unwrapped struct {
	Header []byte
	Plain  []byte
	Level  int
}

// IsGzip は gzip マジックかを判定する。
func IsGzip(head []byte) bool {
	return len(head) >= 3 && head[0] == 0x1f && head[1] == 0x8b && head[2] == 8
}

// TryUnwrap は常に失敗する(CGO 無効)。
func TryUnwrap(orig []byte) (*Unwrapped, bool) { return nil, false }

// Reconstruct は常にエラーを返す(CGO 無効)。
func Reconstruct(header []byte, level int, plain []byte) ([]byte, error) {
	return nil, errors.New("このビルドは precompression 非対応です(CGO 無効)")
}
