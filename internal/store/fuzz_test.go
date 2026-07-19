package store

import (
	"bytes"
	"testing"
)

// FuzzBCJRoundTrip は BCJ x86 フィルタの完全可逆性(任意入力で
// encode→decode が恒等)を検証する。
func FuzzBCJRoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xE8, 0x01, 0x02, 0x03, 0x04, 0xE9, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add(bytes.Repeat([]byte{0xE8}, 32))
	f.Fuzz(func(t *testing.T, data []byte) {
		enc := bcjX86Encode(data)
		if len(enc) != len(data) {
			t.Fatal("BCJ はサイズを変えないはず")
		}
		dec := bcjX86Decode(enc)
		if !bytes.Equal(dec, data) {
			t.Fatal("BCJ 往復不一致")
		}
	})
}

// FuzzBrotliDecode は攻撃者制御の brotli ストリームでの復号がパニック・
// メモリ暴走しないことを検証する(復号エラーは正常系)。
func FuzzBrotliDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add(brotliCompressMax([]byte("seed data for brotli fuzz")))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, err := brotliDecode(data, 1<<20)
		if err == nil && len(out) > 1<<30 {
			t.Fatal("上限を超えて伸長した")
		}
	})
}

func FuzzBzip2Decode(f *testing.F) {
	f.Add([]byte{})
	f.Add(bzip2CompressMax([]byte("seed data for bzip2 fuzz seed data for bzip2 fuzz")))
	f.Fuzz(func(t *testing.T, data []byte) {
		// 任意入力でパニックせず、上限を守ることだけを確認(標準ライブラリ decode)。
		out, err := bzip2Decode(data, 1<<20)
		if err == nil && len(out) > 1<<30 {
			t.Fatal("上限を超えて伸長した")
		}
	})
}
