package precomp

// AAC(Advanced Audio Coding)の可逆再圧縮 — コンテナ/フレーミング層。
//
// スマホ動画の音声トラック・音楽・ポッドキャストの定番。AAC-LC は MDCT
// 係数を 11 種のハフマン符号帳で符号化しており、MP3 と同じ「ハフマン→
// 文脈算術」の手法で縮められる。AAC は MP3 と違いビットリザーバを持たず
// 各フレームが自己完結するので扱いは素直:ADTS ヘッダ(と MP4 の mp4a
// サンプル)を骨格に保持し、raw_data_block を算術へ載せ替える。
//
// 採用前に各フレームを正準ハフマンへ再符号化してビット一致検証し、
// 非準拠・非対応構文(SBR/PS/LTP/USAC 等)は原文へ退避する。

import (
	"bytes"
	"encoding/json"
)

// AACRecipe は AAC(ADTS)再構成レシピ。Chunked = 骨格 + 算術ストリーム。
// フレーム境界・ヘッダ長・sfi は骨格中の ADTS ヘッダから復元時に再解析
// できるので、レシピは「原文退避したフレーム番号」だけを持つ(通常は空)。
type AACRecipe struct {
	SkelN    int   `json:"sn"`
	NumFrame int   `json:"nf"`
	Verbatim []int `json:"vb,omitempty"` // 原文退避フレームの番号(昇順)
}

// AACUnwrapped は分解結果。
type AACUnwrapped struct {
	Chunked []byte
	Recipe  *AACRecipe
}

// TryUnwrapAAC は ADTS AAC-LC を再符号化する。
func TryUnwrapAAC(orig []byte, maxPlain int64) (*AACUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if int64(len(orig)) > maxPlain || len(orig) < 64 || !IsAAC(orig) {
		return nil, false
	}
	frames, ok := parseADTS(orig)
	if !ok {
		return nil, false
	}
	aacInitDec()
	enc := newRangeEncoder()
	models := newAACModels()
	recipe := &AACRecipe{NumFrame: len(frames)}
	var skel []byte
	coded := 0
	for i, f := range frames {
		if f.objType != 2 { // AAC-LC のみ
			return nil, false
		}
		hdr := orig[f.start : f.start+f.hdrLen]
		body := orig[f.start+f.hdrLen : f.start+f.frameLen]
		skel = append(skel, hdr...)
		// 検証パス(算術なし)
		vw := &h264Writer{}
		vs := &aacCaptureSink{r: &h264Reader{b: body}, cw: vw, m: models}
		if f.numRDB == 1 && aacWalkRDB(vs, f.sfi) && !vs.bad &&
			vs.r.pos == len(body)*8 && bytes.Equal(vw.b, body) {
			// 符号化パス(算術)
			es := &aacCaptureSink{r: &h264Reader{b: body}, cw: &h264Writer{}, enc: enc, m: models}
			aacWalkRDB(es, f.sfi)
			if es.bad {
				return nil, false // 決定的なはずなので不整合は放棄
			}
			coded++
		} else {
			skel = append(skel, body...) // 原文退避
			recipe.Verbatim = append(recipe.Verbatim, i)
		}
	}
	if coded == 0 {
		return nil, false
	}
	arith := enc.finish()
	recipe.SkelN = len(skel)
	chunked := make([]byte, 0, len(skel)+len(arith))
	chunked = append(chunked, skel...)
	chunked = append(chunked, arith...)
	rj, err := json.Marshal(recipe)
	if err != nil {
		return nil, false
	}
	// 採用ゲート: 保存形は zstd 越しなので zstd 概算で比較
	zo := jpegProbeEncoder.EncodeAll(orig, nil)
	zc := jpegProbeEncoder.EncodeAll(chunked, nil)
	if len(zc)+len(rj)+48 >= len(zo) {
		return nil, false
	}
	rt, err := ReconstructAAC(recipe, chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		return nil, false
	}
	return &AACUnwrapped{Chunked: chunked, Recipe: recipe}, true
}

// ReconstructAAC はレシピと Chunked から元の ADTS AAC を戻す。
// フレーム境界・ヘッダ長・sfi は骨格中の ADTS ヘッダから再解析する。
func ReconstructAAC(recipe *AACRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.NumFrame <= 0 ||
		recipe.SkelN < 0 || recipe.SkelN > len(chunked) {
		return nil, errH264BadRecipe
	}
	aacInitDec()
	skel := chunked[:recipe.SkelN]
	arith := chunked[recipe.SkelN:]
	dec := newRangeDecoder(arith)
	models := newAACModels()
	vb := map[int]bool{}
	for _, i := range recipe.Verbatim {
		vb[i] = true
	}
	var out []byte
	sp := 0
	for i := 0; i < recipe.NumFrame; i++ {
		if sp+7 > len(skel) {
			return nil, errH264BadRecipe
		}
		// ADTS ヘッダ: protection_absent で 7/9、frame_length・sfi を得る
		h := skel[sp:]
		if h[0] != 0xFF || h[1]&0xF6 != 0xF0 {
			return nil, errH264BadRecipe
		}
		hdrLen := 7
		if h[1]&0x01 == 0 {
			hdrLen = 9
		}
		frameLen := int(h[3]&0x3)<<11 | int(h[4])<<3 | int(h[5]>>5)
		sfi := int(h[2]>>2) & 0xF
		bodyLen := frameLen - hdrLen
		if hdrLen > frameLen || bodyLen < 0 || sp+hdrLen > len(skel) {
			return nil, errH264BadRecipe
		}
		out = append(out, skel[sp:sp+hdrLen]...)
		sp += hdrLen
		if vb[i] {
			if sp+bodyLen > len(skel) {
				return nil, errH264BadRecipe
			}
			out = append(out, skel[sp:sp+bodyLen]...)
			sp += bodyLen
		} else {
			cw := &h264Writer{}
			rs := &aacRebuildSink{dec: dec, cw: cw, m: models}
			if !aacWalkRDB(rs, sfi) || rs.bad || len(cw.b) != bodyLen {
				return nil, errH264BadRecipe
			}
			out = append(out, cw.b...)
		}
	}
	return out, nil
}

// IsAAC は ADTS AAC らしいか(syncword 0xFFF)を判定する。
func IsAAC(head []byte) bool {
	if len(head) < 7 {
		return false
	}
	// 0xFFF sync + layer==00
	if head[0] != 0xFF || head[1]&0xF6 != 0xF0 {
		return false
	}
	// sample_frequency_index が有効(15 は予約)
	sfi := (head[2] >> 2) & 0xF
	return sfi < 13
}

// adtsFrame は 1 ADTS フレームの範囲。
type adtsFrame struct {
	start    int // フレーム先頭(ヘッダ含む)
	hdrLen   int // 7 または 9(CRC あり)
	frameLen int // ヘッダ込みの総バイト
	objType  int // AOT(2=LC)
	sfi      int // sample_frequency_index
	chanCfg  int // channel_configuration
	numRDB   int // raw_data_block 数(通常 1)
}

// parseADTS はストリームを ADTS フレームに分解する。
func parseADTS(data []byte) ([]adtsFrame, bool) {
	var frames []adtsFrame
	p := 0
	n := len(data)
	for p+7 <= n {
		if data[p] != 0xFF || data[p+1]&0xF6 != 0xF0 {
			return nil, false // 同期外れ
		}
		crcAbsent := data[p+1]&0x01 != 0
		objType := int(data[p+2]>>6) + 1
		sfi := int(data[p+2]>>2) & 0xF
		chanCfg := int(data[p+2]&0x1)<<2 | int(data[p+3]>>6)
		frameLen := int(data[p+3]&0x3)<<11 | int(data[p+4])<<3 | int(data[p+5]>>5)
		numRDB := int(data[p+6]&0x3) + 1
		if sfi >= 13 || frameLen < 7 || p+frameLen > n {
			return nil, false
		}
		hdrLen := 7
		if !crcAbsent {
			hdrLen = 9
		}
		if hdrLen > frameLen {
			return nil, false
		}
		frames = append(frames, adtsFrame{
			start: p, hdrLen: hdrLen, frameLen: frameLen,
			objType: objType, sfi: sfi, chanCfg: chanCfg, numRDB: numRDB,
		})
		p += frameLen
	}
	if p != n || len(frames) == 0 {
		return nil, false
	}
	return frames, true
}
