package precomp

// PCM 音声(WAV)の可逆分解。
//
// 動画研究(§4.23)で「既に算術符号された動画は縮まない」と結論したのと対に、
// **非圧縮 PCM 音声は現状 1.0x で raw 保存されている=大きな取りこぼし**。
// WAV の PCM サンプルは時間的に強く相関するので、FLAC/Shorten と同じ発想で
// 各チャンネルに固定線形予測子(次数0〜3、特許フリー)をかけて残差にする。
// 残差は小振幅(ほぼ 0 付近)なので、バイト平面に並べ替えると store の
// zstd/brotli が高率で圧縮でき、似た音源同士では残差の dedup/デルタも効く。
//
// 可逆性: 予測↔逆予測は 2^bits の整数環での完全な全単射(丸めなし)で、
// エンコード/デコードの両方向を自前で持つ(第三者エンコーダとのバイト一致
// 合わせが不要=最も安全な変換)。加えて「復元して元と一致する見込みが元より
// 小さいときだけ採用」+ 読み出し時 SHA-256 検証。純Go・cgo 不要。

import (
	"encoding/binary"
	"errors"
)

// IsWAV は RIFF/WAVE シグネチャを判定する。
func IsWAV(head []byte) bool {
	return len(head) >= 12 &&
		head[0] == 'R' && head[1] == 'I' && head[2] == 'F' && head[3] == 'F' &&
		head[8] == 'W' && head[9] == 'A' && head[10] == 'V' && head[11] == 'E'
}

// WAVRecipe は PCM(WAV/AIFF)再構成レシピ。
type WAVRecipe struct {
	PrefixLen int     `json:"pfx"`          // data サンプル直前までのスケルトン長
	Suffix    []byte  `json:"sfx"`          // サンプル領域以降の原文(data パディング+後続チャンク)
	Channels  int     `json:"ch"`           // チャンネル数
	Bytes     int     `json:"bps"`          // 1サンプルのバイト数(1..4)
	Frames    int     `json:"fr"`           // フレーム数(= サンプル数/ch)
	Orders    []uint8 `json:"ord"`          // 予測次数。BlockSize>0 なら [ch][block] を平坦化、0 ならチャンネルごと
	BigEndian bool    `json:"be,omitempty"` // AIFF は真(サンプルがビッグエンディアン)
	BlockSize int     `json:"bs,omitempty"` // ブロック適応次数のブロック長(0=チャンネル一律)
}

// WAVUnwrapped は分解結果。
type WAVUnwrapped struct {
	Chunked []byte
	Recipe  *WAVRecipe
}

const wavMaxOrder = 3

// TryUnwrapWAV は WAV の PCM サンプルを予測残差のバイト平面に変換する。
func TryUnwrapWAV(orig []byte, maxPlain int64) (*WAVUnwrapped, bool) {
	if len(orig) < 44 || !IsWAV(orig) {
		return nil, false
	}
	ch, bps, dataOff, dataLen, ok := parseWAVFmtData(orig)
	if !ok {
		return nil, false
	}
	return tryUnwrapPCM(orig, ch, bps, dataOff, dataLen, false, maxPlain)
}

// TryUnwrapAIFF は AIFF(ビッグエンディアン PCM)を分解する。
func TryUnwrapAIFF(orig []byte, maxPlain int64) (*WAVUnwrapped, bool) {
	if len(orig) < 44 || !IsAIFF(orig) {
		return nil, false
	}
	ch, bps, dataOff, dataLen, ok := parseAIFFCommSsnd(orig)
	if !ok {
		return nil, false
	}
	return tryUnwrapPCM(orig, ch, bps, dataOff, dataLen, true, maxPlain)
}

// tryUnwrapPCM は WAV/AIFF 共通の変換本体。
func tryUnwrapPCM(orig []byte, ch, bps, dataOff, dataLen int, bigEndian bool, maxPlain int64) (*WAVUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	frameBytes := ch * bps
	if frameBytes == 0 || dataOff < 0 || dataOff+dataLen > len(orig) {
		return nil, false
	}
	frames := dataLen / frameBytes
	sampleBytes := frames * frameBytes
	if frames < 16 || int64(sampleBytes) > maxPlain {
		return nil, false
	}

	mask := uint32(1)<<(uint(bps)*8) - 1
	chans := make([][]uint32, ch)
	for c := 0; c < ch; c++ {
		chans[c] = make([]uint32, frames)
	}
	for f := 0; f < frames; f++ {
		fo := dataOff + f*frameBytes
		for c := 0; c < ch; c++ {
			chans[c][f] = readSample(orig[fo+c*bps:], bps, bigEndian)
		}
	}

	// ブロック適応: チャンネルごとに blockSize サンプル区間で最良次数を選ぶ
	// (連続履歴を保つのでブロック境界でも予測は途切れない)。無音/有音や
	// 静→動で最適次数が変わる音声で縮む。
	blockSize := pcmBlockSize
	nbPerCh := (frames + blockSize - 1) / blockSize
	orders := make([]uint8, 0, ch*nbPerCh)
	resid := make([][]uint32, ch)
	for c := 0; c < ch; c++ {
		ords, res := blockFixedResidual(chans[c], mask, blockSize)
		orders = append(orders, ords...)
		resid[c] = res
	}

	// 残差をバイト平面(チャンネル→バイト位置→フレーム)に並べる。
	payload := make([]byte, sampleBytes)
	pos := 0
	for c := 0; c < ch; c++ {
		for b := 0; b < bps; b++ {
			shift := uint(b * 8)
			for f := 0; f < frames; f++ {
				payload[pos] = byte(resid[c][f] >> shift)
				pos++
			}
		}
	}

	prefix := orig[:dataOff]
	suffix := append([]byte(nil), orig[dataOff+sampleBytes:]...)
	chunked := make([]byte, 0, len(prefix)+len(payload))
	chunked = append(chunked, prefix...)
	chunked = append(chunked, payload...)

	probe := jpegProbeEncoder.EncodeAll(chunked, make([]byte, 0, len(chunked)/2))
	if len(probe)+len(suffix) >= len(orig) {
		return nil, false
	}
	return &WAVUnwrapped{
		Chunked: chunked,
		Recipe: &WAVRecipe{
			PrefixLen: dataOff, Suffix: suffix,
			Channels: ch, Bytes: bps, Frames: frames, Orders: orders, BigEndian: bigEndian,
			BlockSize: blockSize,
		},
	}, true
}

// pcmBlockSize はブロック適応次数のブロック長(サンプル)。
const pcmBlockSize = 4096

// ReconstructWAV はレシピとチャンク化内容から元の WAV をビット単位で戻す。
func ReconstructWAV(recipe *WAVRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.PrefixLen < 0 || recipe.PrefixLen > len(chunked) ||
		recipe.Channels < 1 || recipe.Bytes < 1 || recipe.Bytes > 4 || recipe.Frames < 0 {
		return nil, errors.New("WAV レシピが不正です")
	}
	ch, bps, frames := recipe.Channels, recipe.Bytes, recipe.Frames
	sampleBytes := frames * ch * bps
	prefix := chunked[:recipe.PrefixLen]
	payload := chunked[recipe.PrefixLen:]
	if len(payload) != sampleBytes {
		return nil, errors.New("WAV ペイロード長が不一致")
	}
	mask := uint32(1)<<(uint(bps)*8) - 1

	// バイト平面 → チャンネルごとの残差
	resid := make([][]uint32, ch)
	pos := 0
	for c := 0; c < ch; c++ {
		resid[c] = make([]uint32, frames)
		for b := 0; b < bps; b++ {
			shift := uint(b * 8)
			for f := 0; f < frames; f++ {
				resid[c][f] |= uint32(payload[pos]) << shift
				pos++
			}
		}
	}
	// 逆予測でサンプルを復元
	chans := make([][]uint32, ch)
	if recipe.BlockSize > 0 {
		nb := (frames + recipe.BlockSize - 1) / recipe.BlockSize
		if len(recipe.Orders) != ch*nb {
			return nil, errors.New("WAV ブロック次数の数が不一致")
		}
		for c := 0; c < ch; c++ {
			chans[c] = inverseBlockFixed(resid[c], recipe.Orders[c*nb:(c+1)*nb], mask, recipe.BlockSize)
		}
	} else {
		if len(recipe.Orders) != ch {
			return nil, errors.New("WAV 予測次数が不一致")
		}
		for c := 0; c < ch; c++ {
			chans[c] = inverseFixed(int(recipe.Orders[c]), resid[c], mask)
		}
	}
	// インターリーブして data サンプルを組み立て
	frameBytes := ch * bps
	out := make([]byte, 0, len(prefix)+sampleBytes+len(recipe.Suffix))
	out = append(out, prefix...)
	sbuf := make([]byte, sampleBytes)
	for f := 0; f < frames; f++ {
		fo := f * frameBytes
		for c := 0; c < ch; c++ {
			writeSample(sbuf[fo+c*bps:], chans[c][f], bps, recipe.BigEndian)
		}
	}
	out = append(out, sbuf...)
	out = append(out, recipe.Suffix...)
	return out, nil
}

// ---- 固定予測子(Shorten 系、特許フリー) ----

// blockFixedResidual はチャンネルを blockSize 区間に分け、各区間で最良次数を
// 選ぶ(履歴は連続)。orders は区間ごとの次数、res は全体の残差。
func blockFixedResidual(x []uint32, mask uint32, blockSize int) ([]uint8, []uint32) {
	n := len(x)
	res := make([]uint32, n)
	var orders []uint8
	half := (mask >> 1) + 1
	cost := func(r uint32) uint64 {
		if r >= half {
			return uint64(mask - r + 1)
		}
		return uint64(r)
	}
	for s := 0; s < n; s += blockSize {
		e := s + blockSize
		if e > n {
			e = n
		}
		bestOrd, bestCost := 0, ^uint64(0)
		for ord := 0; ord <= wavMaxOrder; ord++ {
			var cst uint64
			for i := s; i < e; i++ {
				cst += cost((x[i] - predictFixed(ord, x, i)) & mask)
			}
			if cst < bestCost {
				bestCost, bestOrd = cst, ord
			}
		}
		for i := s; i < e; i++ {
			res[i] = (x[i] - predictFixed(bestOrd, x, i)) & mask
		}
		orders = append(orders, uint8(bestOrd))
	}
	return orders, res
}

// inverseBlockFixed は blockFixedResidual の逆。
func inverseBlockFixed(res []uint32, orders []uint8, mask uint32, blockSize int) []uint32 {
	n := len(res)
	x := make([]uint32, n)
	for bi, s := 0, 0; s < n; bi, s = bi+1, s+blockSize {
		e := s + blockSize
		if e > n {
			e = n
		}
		ord := int(orders[bi])
		for i := s; i < e; i++ {
			x[i] = (res[i] + predictFixed(ord, x, i)) & mask
		}
	}
	return x
}

// inverseFixed は残差からサンプルを逐次復元する。
func inverseFixed(ord int, res []uint32, mask uint32) []uint32 {
	x := make([]uint32, len(res))
	for i := range res {
		x[i] = (res[i] + predictFixed(ord, x, i)) & mask
	}
	return x
}

// predictFixed は位置 i の固定予測値(直前サンプルからの外挿)。
// 端(i<ord)では利用可能な範囲に次数を落とす。
func predictFixed(ord int, x []uint32, i int) uint32 {
	if ord > i {
		ord = i
	}
	switch ord {
	case 0:
		return 0
	case 1:
		return x[i-1]
	case 2:
		return 2*x[i-1] - x[i-2]
	default: // 3
		return 3*x[i-1] - 3*x[i-2] + x[i-3]
	}
}

// ---- WAV パース ----

// parseWAVFmtData は fmt チャンクから (ch, bytesPerSample, dataSampleOffset,
// dataLen) を取り出す。整数 PCM(fmt=1 または extensible の PCM SubFormat)
// のみ対応。
func parseWAVFmtData(orig []byte) (ch, bps, dataOff, dataLen int, ok bool) {
	p := 12
	var haveFmt bool
	for p+8 <= len(orig) {
		id := string(orig[p : p+4])
		size := int(binary.LittleEndian.Uint32(orig[p+4 : p+8]))
		body := p + 8
		if size < 0 || body+size > len(orig) {
			return
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return
			}
			audioFmt := int(binary.LittleEndian.Uint16(orig[body:]))
			ch = int(binary.LittleEndian.Uint16(orig[body+2:]))
			bits := int(binary.LittleEndian.Uint16(orig[body+14:]))
			// extensible(0xFFFE): SubFormat の先頭2バイトが実フォーマット
			if audioFmt == 0xFFFE && size >= 26 {
				audioFmt = int(binary.LittleEndian.Uint16(orig[body+24:]))
			}
			if audioFmt != 1 { // PCM 整数のみ(float=3 は対象外)
				return
			}
			if ch < 1 || ch > 8 || bits%8 != 0 || bits < 8 || bits > 32 {
				return
			}
			bps = bits / 8
			haveFmt = true
		case "data":
			if !haveFmt {
				return
			}
			dataOff = body
			dataLen = size
			ok = true
			return
		}
		p = body + size
		if size%2 == 1 {
			p++ // RIFF は偶数境界
		}
	}
	return
}

// readSample はエンディアンに応じて n バイトを uint32 として読む。
func readSample(b []byte, n int, bigEndian bool) uint32 {
	var v uint32
	if bigEndian {
		for i := 0; i < n; i++ {
			v = v<<8 | uint32(b[i])
		}
	} else {
		for i := 0; i < n; i++ {
			v |= uint32(b[i]) << uint(i*8)
		}
	}
	return v
}

// writeSample はエンディアンに応じて n バイトを書く。
func writeSample(b []byte, v uint32, n int, bigEndian bool) {
	if bigEndian {
		for i := 0; i < n; i++ {
			b[i] = byte(v >> uint((n-1-i)*8))
		}
	} else {
		for i := 0; i < n; i++ {
			b[i] = byte(v >> uint(i*8))
		}
	}
}

// IsAIFF は FORM/AIFF(または AIFC)シグネチャを判定する。
func IsAIFF(head []byte) bool {
	return len(head) >= 12 &&
		head[0] == 'F' && head[1] == 'O' && head[2] == 'R' && head[3] == 'M' &&
		head[8] == 'A' && head[9] == 'I' && head[10] == 'F' &&
		(head[11] == 'F' || head[11] == 'C')
}

// parseAIFFCommSsnd は COMM/SSND から (ch, bytesPerSample, sampleOffset,
// sampleLen) を取り出す。非圧縮 PCM(AIFF、または AIFC の 'NONE'/'sowt')のみ。
func parseAIFFCommSsnd(orig []byte) (ch, bps, dataOff, dataLen int, ok bool) {
	aifc := orig[11] == 'C'
	p := 12
	var haveComm bool
	var bits int
	for p+8 <= len(orig) {
		id := string(orig[p : p+4])
		size := int(be32(orig[p+4:]))
		body := p + 8
		if size < 0 || body+size > len(orig) {
			return
		}
		switch id {
		case "COMM":
			if size < 18 {
				return
			}
			ch = int(be16(orig[body:]))
			bits = int(be16(orig[body+6:]))
			// AIFC は圧縮種別が続く(offset 18〜)。'NONE'/'sowt'(=LE PCM)のみ可。
			if aifc {
				if size < 22 {
					return
				}
				comp := string(orig[body+18 : body+22])
				if comp != "NONE" && comp != "sowt" && comp != "twos" {
					return
				}
			}
			if ch < 1 || ch > 8 || bits%8 != 0 || bits < 8 || bits > 32 {
				return
			}
			bps = bits / 8
			haveComm = true
		case "SSND":
			if !haveComm || size < 8 {
				return
			}
			// SSND: offset(4) + blockSize(4) + サンプルデータ
			off := int(be32(orig[body:]))
			dataOff = body + 8 + off
			dataLen = size - 8 - off
			if dataLen < 0 || dataOff+dataLen > len(orig) {
				return
			}
			ok = true
			return
		}
		p = body + size
		if size%2 == 1 {
			p++
		}
	}
	return
}

func be16(b []byte) uint16 { return uint16(b[0])<<8 | uint16(b[1]) }
func be32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
