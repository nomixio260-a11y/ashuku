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
	"math"
)

func absF(x float64) float64   { return math.Abs(x) }
func roundF(x float64) float64 { return math.Round(x) }
func isNaNInf(x float64) bool  { return math.IsNaN(x) || math.IsInf(x, 0) }

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
	Orders    []uint8 `json:"ord"`          // 予測種別。BlockSize>0 なら [ch][block] を平坦化。0〜4=固定次数、0xFF=LPC
	BigEndian bool    `json:"be,omitempty"` // AIFF は真(サンプルがビッグエンディアン)
	BlockSize int     `json:"bs,omitempty"` // ブロック適応次数のブロック長(0=チャンネル一律)
	// LPCData は LPC ブロックのパラメータをブロック順に連結したもの。
	// 各 LPC ブロック: shift(1) + order*int16LE 係数。
	LPCData []byte `json:"lpc,omitempty"`
}

// WAVUnwrapped は分解結果。
type WAVUnwrapped struct {
	Chunked []byte
	Recipe  *WAVRecipe
}

const wavMaxOrder = 4 // FLAC 固定予測子は 0〜4

// LPC 設定
const (
	lpcOrder  = 8    // 適応線形予測の次数
	lpcQBits  = 12   // 係数の量子化ビット幅(符号付き)
	lpcMarker = 0xFF // Orders 内で「このブロックは LPC」を表す値
)

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

	// ブロック適応の予測を2通り作って実測で選ぶ:
	//  (a) 固定予測(次数0〜4)のみ — 残差に構造が残り byte-plane+zstd と相性が良い
	//  (b) 固定 + LPC(適応線形予測、次数8)の best-of — 残差の振幅は小さいが白色化
	// LPC は残差振幅を下げるが zstd の LZ で拾える構造を消すため、音源により
	// (a)/(b) の優劣が逆転する。両方をバイト平面化して probe 圧縮し小さい方を採る。
	blockSize := pcmBlockSize
	prefix := orig[:dataOff]
	suffix := orig[dataOff+sampleBytes:]

	buildCand := func(allowLPC bool) (chunked []byte, orders []uint8, lpcData []byte) {
		nbPerCh := (frames + blockSize - 1) / blockSize
		orders = make([]uint8, 0, ch*nbPerCh)
		resid := make([][]uint32, ch)
		for c := 0; c < ch; c++ {
			ords, res, lpc := blockResidual(chans[c], mask, bps, blockSize, allowLPC)
			orders = append(orders, ords...)
			lpcData = append(lpcData, lpc...)
			resid[c] = res
		}
		payload := make([]byte, sampleBytes)
		pos := 0
		for c := 0; c < ch; c++ {
			for b := 0; b < bps; b++ {
				sh := uint(b * 8)
				for f := 0; f < frames; f++ {
					payload[pos] = byte(resid[c][f] >> sh)
					pos++
				}
			}
		}
		chunked = make([]byte, 0, len(prefix)+len(payload))
		chunked = append(chunked, prefix...)
		chunked = append(chunked, payload...)
		return chunked, orders, lpcData
	}

	fixedChunk, fixedOrders, _ := buildCand(false)
	lpcChunk, lpcOrders, lpcData := buildCand(true)
	fixedProbe := len(jpegProbeEncoder.EncodeAll(fixedChunk, make([]byte, 0, len(fixedChunk)/2)))
	lpcProbe := len(jpegProbeEncoder.EncodeAll(lpcChunk, make([]byte, 0, len(lpcChunk)/2)))

	chunked, orders := fixedChunk, fixedOrders
	best := fixedProbe
	lpcOut := []byte(nil)
	if lpcProbe < best {
		chunked, orders, lpcOut, best = lpcChunk, lpcOrders, lpcData, lpcProbe
	}
	if best+len(suffix) >= len(orig) {
		return nil, false
	}
	return &WAVUnwrapped{
		Chunked: chunked,
		Recipe: &WAVRecipe{
			PrefixLen: dataOff, Suffix: append([]byte(nil), suffix...),
			Channels: ch, Bytes: bps, Frames: frames, Orders: orders, BigEndian: bigEndian,
			BlockSize: blockSize, LPCData: lpcOut,
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
		lpc := recipe.LPCData
		for c := 0; c < ch; c++ {
			var err error
			chans[c], lpc, err = inverseBlock(resid[c], recipe.Orders[c*nb:(c+1)*nb], mask, bps, recipe.BlockSize, lpc)
			if err != nil {
				return nil, err
			}
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

// signExt は b バイト無符号値を符号付き int64 に拡張する。
func signExt(v uint32, bps int) int64 {
	bits := uint(bps * 8)
	half := uint32(1) << (bits - 1)
	if v >= half {
		return int64(v) - (int64(1) << bits)
	}
	return int64(v)
}

func residCostOf(r, mask uint32) uint64 {
	half := (mask >> 1) + 1
	if r >= half {
		return uint64(mask - r + 1)
	}
	return uint64(r)
}

// blockResidual はチャンネルを blockSize 区間に分け、各区間で固定予測(0〜4)と
// LPC の best-of を選ぶ(履歴は連続)。orders は区間ごとの種別(0〜4 or 0xFF)、
// res は全体の残差、lpcData は LPC ブロックのパラメータ連結。
func blockResidual(x []uint32, mask uint32, bps, blockSize int, allowLPC bool) ([]uint8, []uint32, []byte) {
	n := len(x)
	res := make([]uint32, n)
	var orders []uint8
	var lpcData []byte
	for s := 0; s < n; s += blockSize {
		e := s + blockSize
		if e > n {
			e = n
		}
		// 固定予測の最良次数
		bestOrd, bestCost := 0, ^uint64(0)
		for ord := 0; ord <= wavMaxOrder; ord++ {
			var cst uint64
			for i := s; i < e; i++ {
				cst += residCostOf((x[i]-predictFixed(ord, x, i))&mask, mask)
			}
			if cst < bestCost {
				bestCost, bestOrd = cst, ord
			}
		}
		// LPC を試す(allowLPC のときのみ)
		qc, shift, ok := lpcQuantize(x, s, e, bps)
		useLPC := false
		var lpcCost uint64
		if allowLPC && ok {
			for i := s; i < e; i++ {
				lpcCost += residCostOf((x[i]-lpcPredict(x, i, s, qc, shift, bps))&mask, mask)
			}
			if lpcCost < bestCost {
				useLPC = true
			}
		}
		if useLPC {
			for i := s; i < e; i++ {
				res[i] = (x[i] - lpcPredict(x, i, s, qc, shift, bps)) & mask
			}
			orders = append(orders, lpcMarker)
			lpcData = append(lpcData, byte(shift))
			for _, c := range qc {
				lpcData = append(lpcData, byte(uint16(c)), byte(uint16(c)>>8))
			}
		} else {
			for i := s; i < e; i++ {
				res[i] = (x[i] - predictFixed(bestOrd, x, i)) & mask
			}
			orders = append(orders, uint8(bestOrd))
		}
	}
	return orders, res, lpcData
}

// inverseBlock は blockResidual の逆。消費した lpcData の残りを返す。
func inverseBlock(res []uint32, orders []uint8, mask uint32, bps, blockSize int, lpcData []byte) ([]uint32, []byte, error) {
	n := len(res)
	x := make([]uint32, n)
	for bi, s := 0, 0; s < n; bi, s = bi+1, s+blockSize {
		e := s + blockSize
		if e > n {
			e = n
		}
		if orders[bi] == lpcMarker {
			if len(lpcData) < 1+lpcOrder*2 {
				return nil, nil, errors.New("WAV LPC データが不足")
			}
			shift := int(lpcData[0])
			qc := make([]int32, lpcOrder)
			for j := 0; j < lpcOrder; j++ {
				qc[j] = int32(int16(uint16(lpcData[1+j*2]) | uint16(lpcData[2+j*2])<<8))
			}
			lpcData = lpcData[1+lpcOrder*2:]
			for i := s; i < e; i++ {
				x[i] = (res[i] + lpcPredict(x, i, s, qc, shift, bps)) & mask
			}
		} else {
			ord := int(orders[bi])
			for i := s; i < e; i++ {
				x[i] = (res[i] + predictFixed(ord, x, i)) & mask
			}
		}
	}
	return x, lpcData, nil
}

// lpcPredict は位置 i の LPC 予測値(mod 2^b)。ブロック先頭 s から order 本
// 未満は 0 予測(残差=サンプルそのもの)にして端を単純化する。
func lpcPredict(x []uint32, i, s int, qc []int32, shift, bps int) uint32 {
	if i-s < len(qc) {
		return 0
	}
	var acc int64
	for j := 0; j < len(qc); j++ {
		acc += int64(qc[j]) * signExt(x[i-1-j], bps)
	}
	return uint32(acc >> uint(shift))
}

// lpcQuantize はブロック [s,e) の LPC 係数を求めて量子化する。
// 返り値: 量子化係数(order 本)、右シフト量、成功可否。
func lpcQuantize(x []uint32, s, e, bps int) ([]int32, int, bool) {
	n := e - s
	if n <= lpcOrder*2 {
		return nil, 0, false
	}
	// 自己相関(符号付きサンプル)
	ac := make([]float64, lpcOrder+1)
	for lag := 0; lag <= lpcOrder; lag++ {
		var sum float64
		for i := s + lag; i < e; i++ {
			sum += float64(signExt(x[i], bps)) * float64(signExt(x[i-lag], bps))
		}
		ac[lag] = sum
	}
	if ac[0] == 0 {
		return nil, 0, false
	}
	// Levinson-Durbin
	lpc := make([]float64, lpcOrder)
	errPow := ac[0]
	for i := 0; i < lpcOrder; i++ {
		r := -ac[i+1]
		for j := 0; j < i; j++ {
			r -= lpc[j] * ac[i-j]
		}
		r /= errPow
		lpc[i] = r
		for j := 0; j < i/2; j++ {
			t := lpc[j]
			lpc[j] += r * lpc[i-1-j]
			lpc[i-1-j] += r * t
		}
		if i&1 == 1 {
			lpc[i/2] += lpc[i/2] * r
		}
		errPow *= 1 - r*r
		if errPow <= 0 {
			return nil, 0, false
		}
	}
	// 予測子は -lpc(pred = -Σ lpc[j]*x[i-1-j])。量子化。
	maxc := 0.0
	for _, c := range lpc {
		if a := absF(c); a > maxc {
			maxc = a
		}
	}
	if maxc == 0 || isNaNInf(maxc) {
		return nil, 0, false
	}
	// shift: 係数が qBits 符号付きに収まる最大シフト
	shift := lpcQBits - 1
	for (maxc*float64(int64(1)<<uint(shift))) >= float64(int64(1)<<(lpcQBits-1)) && shift > 0 {
		shift--
	}
	qc := make([]int32, lpcOrder)
	lim := int32(1)<<(lpcQBits-1) - 1
	for j := range lpc {
		q := int32(roundF(-lpc[j] * float64(int64(1)<<uint(shift))))
		if q > lim {
			q = lim
		}
		if q < -lim-1 {
			q = -lim - 1
		}
		qc[j] = q
	}
	return qc, shift, true
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
	case 3:
		return 3*x[i-1] - 3*x[i-2] + x[i-3]
	default: // 4
		return 4*x[i-1] - 6*x[i-2] + 4*x[i-3] - x[i-4]
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
