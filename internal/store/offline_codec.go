package store

// オフライン経路の追加コーデック(brotli)と BCJ フィルタ。
//
// 実測(RESEARCH.md §4.20)で brotli 品質11(大窓)は libzstd-22+LDM を
// テキスト・JSON・CSV・バイナリ・ソースの全種別で下回った(相対 -1〜-4%)。
// 圧縮は数倍遅いが伸長は zstd 並みに速いため、「圧縮は一度だけ・読みは
// 何度でも」のオフライン再圧縮(Optimize)にだけ使う。採用は常に
// 「複数候補を実測して最小を選ぶ」ベストオブ方式で、悪化はあり得ない。
//
// BCJ(Branch/Call/Jump)フィルタは x86 の CALL/JMP 相対アドレスを絶対
// アドレスに変換してから圧縮する(xz の x86 フィルタと同じ発想)。同じ関数
// への呼び出しが同じバイト列になり LZ が拾えるようになるため、実行バイナリ
// でさらに -1.5% 前後縮む。変換は完全可逆で、採用前に往復検証も行う。

import (
	"bytes"
	"io"

	"github.com/andybalholm/brotli"
)

// 追加の保存表現タグ(ChunkMeta.Compression)。
const (
	compressionBr      = "br"       // brotli 品質11
	compressionZstdBCJ = "zstd-bcj" // BCJ 変換 + zstd
	compressionBrBCJ   = "br-bcj"   // BCJ 変換 + brotli
)

// independentComp は「単体で完結する保存表現」(デルタ・リージョン参照で
// ない)かを返す。リージョン化・パック化の対象判定に使う。
func independentComp(c string) bool {
	switch c {
	case compressionZstd, compressionRaw, compressionBr, compressionZstdBCJ, compressionBrBCJ:
		return true
	}
	return false
}

// brotliCompressMax は brotli 品質11・大窓(lgwin=24)で圧縮する。
func brotliCompressMax(data []byte) []byte {
	var buf bytes.Buffer
	w := brotli.NewWriterOptions(&buf, brotli.WriterOptions{Quality: 11, LGWin: 24})
	if _, err := w.Write(data); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// brotliDecode は brotli ストリームを伸長する。sizeHint は展開サイズの
// 見込み(容量事前確保)。リージョン読みでは下限見積りのことがあるため
// 上限には使わず、絶対上限(1GiB)だけをメモリ保護として課す
// (zstd 経路の意味論と同じ。内容の完全性は SHA-256 検証が担う)。
func brotliDecode(stored []byte, sizeHint int64) ([]byte, error) {
	const hardCap = 1 << 30
	r := brotli.NewReader(bytes.NewReader(stored))
	out := make([]byte, 0, sizeHint)
	buf := make([]byte, 64<<10)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if int64(len(out)) > hardCap {
			return nil, io.ErrUnexpectedEOF
		}
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// ---- BCJ x86 フィルタ(E8/E9 の32bit相対→絶対変換) ----
//
// 走査は「E8/E9 を見たら5バイト進む、それ以外は1バイト進む」。変換で書き
// 換わるのはオペランドの4バイトだけで、走査の判定に使うバイト(オペコード
// 位置)は不変のため、エンコードとデコードは完全に同じ経路をたどる=可逆。

func bcjX86Encode(data []byte) []byte {
	out := append([]byte(nil), data...)
	for i := 0; i+5 <= len(out); {
		if op := out[i]; op == 0xE8 || op == 0xE9 {
			rel := int32(uint32(out[i+1]) | uint32(out[i+2])<<8 | uint32(out[i+3])<<16 | uint32(out[i+4])<<24)
			abs := uint32(int32(i)+5) + uint32(rel)
			out[i+1] = byte(abs)
			out[i+2] = byte(abs >> 8)
			out[i+3] = byte(abs >> 16)
			out[i+4] = byte(abs >> 24)
			i += 5
		} else {
			i++
		}
	}
	return out
}

func bcjX86Decode(data []byte) []byte {
	out := append([]byte(nil), data...)
	for i := 0; i+5 <= len(out); {
		if op := out[i]; op == 0xE8 || op == 0xE9 {
			abs := uint32(out[i+1]) | uint32(out[i+2])<<8 | uint32(out[i+3])<<16 | uint32(out[i+4])<<24
			rel := abs - uint32(int32(i)+5)
			out[i+1] = byte(rel)
			out[i+2] = byte(rel >> 8)
			out[i+3] = byte(rel >> 16)
			out[i+4] = byte(rel >> 24)
			i += 5
		} else {
			i++
		}
	}
	return out
}

// bcjWorthy は BCJ 変換を試す価値がありそうか(x86 機械語らしいか)の
// ヒューリスティック。誤検知しても best-of 判定で悪化は採用されないため、
// これは CPU の節約のためだけの門番。
func bcjWorthy(data []byte) bool {
	if len(data) < 4096 {
		return false
	}
	sample := data
	if len(sample) > 1<<20 {
		sample = sample[:1<<20]
	}
	var e8, nul int
	for _, b := range sample {
		switch b {
		case 0xE8, 0xE9:
			e8++
		case 0x00:
			nul++
		}
	}
	n := len(sample)
	// 機械語は NUL(上位バイト・パディング)が多く、E8/E9 が適度に出る。
	return nul*100 >= n*2 && e8*1000 >= n*1 && e8*100 <= n*8
}

// offlineCompressBest はオフライン経路の最終圧縮: zstd 最強・brotli・
// (機械語らしければ)BCJ 変換併用の各候補を実測し、最小の表現を返す。
// 返り値は (保存バイト, Compression タグ, 表現ID)。zstd 以外の候補は
// 採用前に往復検証し、失敗するものは決して採用しない。
func (s *Store) offlineCompressBest(data []byte) ([]byte, string, string) {
	best := s.maxCompress(data)
	comp, rep := compressionZstd, "z22"
	if br := brotliCompressMax(data); br != nil && len(br) < len(best) {
		if rt, err := brotliDecode(br, int64(len(data))); err == nil && bytes.Equal(rt, data) {
			best, comp, rep = br, compressionBr, "b11"
		}
	}
	if bcjWorthy(data) {
		t := bcjX86Encode(data)
		if bytes.Equal(bcjX86Decode(t), data) {
			if z := s.maxCompress(t); len(z) < len(best) {
				best, comp, rep = z, compressionZstdBCJ, "z22j"
			}
			if br := brotliCompressMax(t); br != nil && len(br) < len(best) {
				if rt, err := brotliDecode(br, int64(len(t))); err == nil && bytes.Equal(rt, t) {
					best, comp, rep = br, compressionBrBCJ, "b11j"
				}
			}
		}
	}
	return best, comp, rep
}
