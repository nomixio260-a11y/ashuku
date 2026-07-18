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

// WAVRecipe は WAV 再構成レシピ。
type WAVRecipe struct {
	PrefixLen int     `json:"pfx"` // data サンプル直前までのスケルトン長
	Suffix    []byte  `json:"sfx"` // サンプル領域以降の原文(data パディング+後続チャンク)
	Channels  int     `json:"ch"`  // チャンネル数
	Bytes     int     `json:"bps"` // 1サンプルのバイト数(1..4)
	Frames    int     `json:"fr"`  // フレーム数(= サンプル数/ch)
	Orders    []uint8 `json:"ord"` // チャンネルごとの固定予測次数
}

// WAVUnwrapped は分解結果。
type WAVUnwrapped struct {
	Chunked []byte
	Recipe  *WAVRecipe
}

const wavMaxOrder = 3

// TryUnwrapWAV は WAV の PCM サンプルを予測残差のバイト平面に変換する。
func TryUnwrapWAV(orig []byte, maxPlain int64) (*WAVUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < 44 || !IsWAV(orig) {
		return nil, false
	}
	ch, bps, dataOff, dataLen, ok := parseWAVFmtData(orig)
	if !ok {
		return nil, false
	}
	frameBytes := ch * bps
	if frameBytes == 0 {
		return nil, false
	}
	frames := dataLen / frameBytes
	sampleBytes := frames * frameBytes
	if frames < 16 || int64(sampleBytes) > maxPlain {
		return nil, false // 小さすぎ or 大きすぎ
	}

	// チャンネルごとにサンプルを取り出す(b ビット無符号値として mod 2^b で扱う)。
	mask := uint32(1)<<(uint(bps)*8) - 1
	chans := make([][]uint32, ch)
	for c := 0; c < ch; c++ {
		chans[c] = make([]uint32, frames)
	}
	for f := 0; f < frames; f++ {
		fo := dataOff + f*frameBytes
		for c := 0; c < ch; c++ {
			chans[c][f] = readLE(orig[fo+c*bps:], bps)
		}
	}

	// 各チャンネルで最良の固定次数を選び、残差(mod 2^b)を求める。
	orders := make([]uint8, ch)
	resid := make([][]uint32, ch)
	for c := 0; c < ch; c++ {
		orders[c], resid[c] = bestFixedResidual(chans[c], mask)
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

	// 採用判定: バイト平面を zstd 最高レベルで probe し、元より小さい時だけ。
	probe := jpegProbeEncoder.EncodeAll(chunked, make([]byte, 0, len(chunked)/2))
	if len(probe)+len(suffix) >= len(orig) {
		return nil, false
	}
	return &WAVUnwrapped{
		Chunked: chunked,
		Recipe: &WAVRecipe{
			PrefixLen: dataOff, Suffix: suffix,
			Channels: ch, Bytes: bps, Frames: frames, Orders: orders,
		},
	}, true
}

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
	if len(recipe.Orders) != ch {
		return nil, errors.New("WAV 予測次数が不一致")
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
	for c := 0; c < ch; c++ {
		chans[c] = inverseFixed(int(recipe.Orders[c]), resid[c], mask)
	}
	// インターリーブして data サンプルを組み立て
	frameBytes := ch * bps
	out := make([]byte, 0, len(prefix)+sampleBytes+len(recipe.Suffix))
	out = append(out, prefix...)
	sbuf := make([]byte, sampleBytes)
	for f := 0; f < frames; f++ {
		fo := f * frameBytes
		for c := 0; c < ch; c++ {
			writeLE(sbuf[fo+c*bps:], chans[c][f], bps)
		}
	}
	out = append(out, sbuf...)
	out = append(out, recipe.Suffix...)
	return out, nil
}

// ---- 固定予測子(Shorten 系、特許フリー) ----

// bestFixedResidual は次数0〜3を試し、残差の絶対値和が最小の次数を選ぶ。
func bestFixedResidual(x []uint32, mask uint32) (uint8, []uint32) {
	bestOrd := 0
	var bestCost uint64 = ^uint64(0)
	var bestRes []uint32
	for ord := 0; ord <= wavMaxOrder; ord++ {
		res := fixedResidual(ord, x, mask)
		var cost uint64
		half := (mask >> 1) + 1
		for _, r := range res {
			// 符号付き振幅で評価
			if r >= half {
				cost += uint64(mask - r + 1)
			} else {
				cost += uint64(r)
			}
			if cost >= bestCost {
				break
			}
		}
		if cost < bestCost {
			bestCost, bestOrd, bestRes = cost, ord, res
		}
	}
	return uint8(bestOrd), bestRes
}

// fixedResidual は次数 ord の固定予測残差(mod 2^b)を返す。
// 予測子: pred = Σ c_k x[i-1-k]、係数は二項係数の交代和。
func fixedResidual(ord int, x []uint32, mask uint32) []uint32 {
	res := make([]uint32, len(x))
	for i := range x {
		res[i] = (x[i] - predictFixed(ord, x, i)) & mask
	}
	return res
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

func readLE(b []byte, n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		v |= uint32(b[i]) << uint(i*8)
	}
	return v
}

func writeLE(b []byte, v uint32, n int) {
	for i := 0; i < n; i++ {
		b[i] = byte(v >> uint(i*8))
	}
}
