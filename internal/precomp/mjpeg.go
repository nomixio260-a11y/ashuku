package precomp

// MJPEG(AVI コンテナ)の可逆分解。
//
// 動画の圧縮研究(RESEARCH §4.23)の結論: H.264/H.265/VP9/AV1 は算術符号
// (CABAC 等)で既に情報理論的限界近くにあり、可逆再圧縮の余地は実質ない。
// 一方 **MJPEG**(Motion JPEG)は「baseline JPEG の連続」であり、監視カメラ・
// ウェブカメラ・ドローン・科学計測で現役の形式。各フレームに JPEG 文脈算術
// コーダ(§4.18-19、実写真 -29〜-34%)をそのまま適用できる=動画で唯一の
// 実装可能な突破口。
//
// AVI(RIFF)コンテナを歩き、movi LIST 内の JPEG フレーム(##dc/##db)を
// フレーム単位で TryUnwrapJPEG(ビット一致検証つき)にかける。一致しない
// フレームはスケルトンに残す(部分適用)。復元時は SHA-256 最終検証(store 層)。

import (
	"encoding/binary"
	"errors"
)

// IsAVI は RIFF/AVI シグネチャを判定する(12バイト必要)。
func IsAVI(head []byte) bool {
	return len(head) >= 12 &&
		head[0] == 'R' && head[1] == 'I' && head[2] == 'F' && head[3] == 'F' &&
		head[8] == 'A' && head[9] == 'V' && head[10] == 'I' && head[11] == ' '
}

// MJPEGSegment はチャンク化ストリームの1区間:
// スケルトン原文 SkelLen バイト + JPEG フレームの分解ペイロード PayloadLen バイト。
type MJPEGSegment struct {
	SkelLen    int64       `json:"s"`
	PayloadLen int64       `json:"p,omitempty"`
	Jpeg       *JPEGRecipe `json:"j,omitempty"`
}

// MJPEGRecipe は AVI/MJPEG 再構成レシピ。
type MJPEGRecipe struct {
	Segments []MJPEGSegment `json:"segs"`
}

// MJPEGUnwrapped は分解結果。
type MJPEGUnwrapped struct {
	Chunked []byte
	Recipe  *MJPEGRecipe
}

// mjpegMaxFrames は処理するフレーム数の上限(暴走防止)。
const mjpegMaxFrames = 100000

// TryUnwrapAVI は AVI コンテナ内の JPEG フレームを分解する。
// 1フレームも分解できない・見込みサイズが元以上なら不採用。
func TryUnwrapAVI(orig []byte, maxPlain int64) (*MJPEGUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < 64 || !IsAVI(orig) {
		return nil, false
	}
	riffSize := int64(binary.LittleEndian.Uint32(orig[4:8]))
	if riffSize < 4 || 8+riffSize > int64(len(orig))+1 { // +1: 奇数サイズの丸め許容
		return nil, false
	}

	var chunked []byte
	var segs []MJPEGSegment
	skelStart := int64(0)
	var plainTotal int64
	unwrapped := 0
	frames := 0

	// walk は RIFF チャンク列 [p, end) を歩く。movi LIST 内のみ frame 判定。
	var walk func(p, end int64, inMovi bool) bool
	walk = func(p, end int64, inMovi bool) bool {
		for p+8 <= end {
			id := orig[p : p+4]
			size := int64(binary.LittleEndian.Uint32(orig[p+4 : p+8]))
			body := p + 8
			if size < 0 || body+size > end {
				return false // 壊れた構造 → 全体を素通し
			}
			if string(id) == "LIST" && size >= 4 {
				sub := string(orig[body : body+4])
				if !walk(body+4, body+size, inMovi || sub == "movi") {
					return false
				}
			} else if inMovi && frames < mjpegMaxFrames && size >= 128 &&
				len(id) == 4 && (id[2] == 'd' && (id[3] == 'c' || id[3] == 'b')) {
				frames++
				payload := orig[body : body+size]
				if IsJPEG(payload) && plainTotal < maxPlain {
					if u, ok := TryUnwrapJPEG(payload, maxPlain-plainTotal); ok {
						// 採用: スケルトンはフレームデータ直前まで
						chunked = append(chunked, orig[skelStart:body]...)
						chunked = append(chunked, u.Chunked...)
						segs = append(segs, MJPEGSegment{
							SkelLen:    body - skelStart,
							PayloadLen: int64(len(u.Chunked)),
							Jpeg:       u.Recipe,
						})
						skelStart = body + size
						plainTotal += int64(len(u.Chunked))
						unwrapped++
					}
				}
			}
			p = body + size
			if size%2 == 1 {
				p++ // RIFF は偶数境界にパディング
			}
		}
		return true
	}
	if !walk(12, int64(len(orig)), false) {
		return nil, false
	}
	if unwrapped == 0 {
		return nil, false
	}
	chunked = append(chunked, orig[skelStart:]...)
	segs = append(segs, MJPEGSegment{SkelLen: int64(len(orig)) - skelStart})

	// 見込み判定: 各フレームは TryUnwrapJPEG が「圧縮見込みが元フレーム
	// 未満」を保証済みだが、GIF と同様に全体でも probe して元より小さく
	// なる場合のみ採用する(スケルトン・レシピぶんの安全マージン)。
	probe := jpegProbeEncoder.EncodeAll(chunked, make([]byte, 0, len(chunked)/2))
	if len(probe) >= len(orig) {
		return nil, false
	}
	return &MJPEGUnwrapped{Chunked: chunked, Recipe: &MJPEGRecipe{Segments: segs}}, true
}

// ReconstructAVI はレシピとチャンク化内容から元の AVI をビット単位で戻す。
func ReconstructAVI(recipe *MJPEGRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil {
		return nil, errors.New("AVI レシピが不正です")
	}
	out := make([]byte, 0, len(chunked))
	off := int64(0)
	for _, seg := range recipe.Segments {
		if seg.SkelLen < 0 || seg.PayloadLen < 0 || off+seg.SkelLen+seg.PayloadLen > int64(len(chunked)) {
			return nil, errors.New("AVI レシピが範囲外です")
		}
		out = append(out, chunked[off:off+seg.SkelLen]...)
		off += seg.SkelLen
		if seg.PayloadLen == 0 {
			continue
		}
		frame, err := ReconstructJPEG(seg.Jpeg, chunked[off:off+seg.PayloadLen])
		if err != nil {
			return nil, err
		}
		off += seg.PayloadLen
		out = append(out, frame...)
	}
	return out, nil
}
