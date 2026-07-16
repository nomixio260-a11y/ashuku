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

// IsZlib は zlib ストリームのマジックかを判定する。
func IsZlib(head []byte) bool {
	if len(head) < 2 {
		return false
	}
	cmf, flg := head[0], head[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && flg&0x20 == 0 &&
		(uint32(cmf)*256+uint32(flg))%31 == 0
}

// Member はマルチメンバー gzip の1メンバーのレシピ。
type Member struct {
	Header   []byte `json:"header"`
	Level    int    `json:"level"`
	PlainLen int64  `json:"plain_len"`
}

var errNoCGO = errors.New("このビルドは precompression 非対応です(CGO 無効)")

// TryUnwrap は常に失敗する(CGO 無効)。
func TryUnwrap(orig []byte, maxPlain int64) (*Unwrapped, bool) { return nil, false }

// TryUnwrapZlib は常に失敗する(CGO 無効)。
func TryUnwrapZlib(orig []byte, maxPlain int64) (*Unwrapped, bool) { return nil, false }

// TryUnwrapGzipMulti は常に失敗する(CGO 無効)。
func TryUnwrapGzipMulti(orig []byte, maxPlain int64) ([]byte, []Member, bool) {
	return nil, nil, false
}

// Reconstruct は常にエラーを返す(CGO 無効)。
func Reconstruct(header []byte, level int, plain []byte) ([]byte, error) {
	return nil, errNoCGO
}

// ReconstructZlib は常にエラーを返す(CGO 無効)。
func ReconstructZlib(header []byte, level int, plain []byte) ([]byte, error) {
	return nil, errNoCGO
}

// ReconstructGzipMulti は常にエラーを返す(CGO 無効)。
func ReconstructGzipMulti(members []Member, plain []byte) ([]byte, error) {
	return nil, errNoCGO
}
