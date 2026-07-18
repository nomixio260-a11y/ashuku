package precomp

// JPEG baseline のエントロピー復号・再符号化と、係数平面へのシリアライズ。
// jpeg.go の構造解析と合わせて可逆再圧縮を構成する。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/bits"

	"github.com/klauspost/compress/zstd"
)

// decodeScan は entropy バイト列を復号し、成分ごとの係数(DC は差分、
// AC は値)を zigzag スキャン位置 k=0..63 の順で返す。
// coeff[c] の長さは nBlocks[c]*64。
func (f *jpegFrame) decodeScan(entropy []byte) ([][]int16, error) {
	for ci := range f.comps {
		c := &f.comps[ci]
		if f.dc[c.dcTable] == nil || f.ac[c.acTable] == nil {
			return nil, errors.New("参照する Huffman テーブルがありません")
		}
	}
	nMCU := f.mcuX * f.mcuY
	coeff := make([][]int16, len(f.comps))
	blkIdx := make([]int, len(f.comps))
	for ci := range f.comps {
		coeff[ci] = make([]int16, nMCU*f.comps[ci].blocksMC*64)
	}
	r := &bitReader{data: entropy}
	pred := make([]int, len(f.comps))
	for mcu := 0; mcu < nMCU; mcu++ {
		if f.restart > 0 && mcu > 0 && mcu%f.restart == 0 {
			if !r.takeRestart() {
				return nil, errors.New("リスタートマーカが見つかりません")
			}
			for i := range pred {
				pred[i] = 0
			}
		}
		for ci := range f.comps {
			c := &f.comps[ci]
			dcT, acT := f.dc[c.dcTable], f.ac[c.acTable]
			for b := 0; b < c.blocksMC; b++ {
				base := blkIdx[ci] * 64
				blkIdx[ci]++
				// DC
				s, err := r.decodeHuff(dcT)
				if err != nil {
					return nil, err
				}
				if s > 11 {
					return nil, errors.New("DC カテゴリが不正")
				}
				diff := extend(r.readBits(int(s)), int(s))
				pred[ci] += diff
				coeff[ci][base] = int16(diff) // DC は差分を保存(そのまま再符号化に使える)
				// AC
				k := 1
				for k < 64 {
					rs, err := r.decodeHuff(acT)
					if err != nil {
						return nil, err
					}
					run := int(rs >> 4)
					size := int(rs & 0x0F)
					if size == 0 {
						if run == 15 {
							k += 16
							continue
						}
						break // EOB
					}
					k += run
					if k > 63 {
						return nil, errors.New("AC 係数位置が範囲外")
					}
					coeff[ci][base+k] = int16(extend(r.readBits(size), size))
					k++
				}
			}
		}
	}
	return coeff, nil
}

// ---- ビットライタ(バイトスタッフィング・1埋めパディング) ----
type bitWriter struct {
	out []byte
	acc uint32
	n   int
}

func (w *bitWriter) writeBits(code uint32, size int) {
	if size == 0 {
		return
	}
	w.acc = (w.acc << uint(size)) | (code & ((1 << uint(size)) - 1))
	w.n += size
	for w.n >= 8 {
		w.n -= 8
		b := byte(w.acc >> uint(w.n))
		w.out = append(w.out, b)
		if b == 0xFF {
			w.out = append(w.out, 0x00) // スタッフィング
		}
	}
}

// pad はバイト境界まで 1 ビットで埋める(JPEG のパディング規約)。
func (w *bitWriter) pad() {
	if w.n%8 != 0 {
		p := 8 - w.n%8
		w.writeBits((1<<uint(p))-1, p)
	}
}

func magCat(v int) int {
	if v < 0 {
		v = -v
	}
	return bits.Len(uint(v))
}

func mantissa(v, s int) uint32 {
	if v < 0 {
		v += (1 << uint(s)) - 1
	}
	return uint32(v) & ((1 << uint(s)) - 1)
}

// encodeScan は係数を標準 baseline アルゴリズムで再符号化する。
func (f *jpegFrame) encodeScan(coeff [][]int16) []byte {
	w := &bitWriter{}
	blkIdx := make([]int, len(f.comps))
	nMCU := f.mcuX * f.mcuY
	rst := 0
	for mcu := 0; mcu < nMCU; mcu++ {
		if f.restart > 0 && mcu > 0 && mcu%f.restart == 0 {
			w.pad()
			w.out = append(w.out, 0xFF, byte(mRST0+rst))
			rst = (rst + 1) & 7
		}
		for ci := range f.comps {
			c := &f.comps[ci]
			dcT, acT := f.dc[c.dcTable], f.ac[c.acTable]
			for b := 0; b < c.blocksMC; b++ {
				base := blkIdx[ci] * 64
				blkIdx[ci]++
				blk := coeff[ci][base : base+64]
				// DC(差分そのもの)
				diff := int(blk[0])
				s := magCat(diff)
				w.writeBits(uint32(dcT.encCode[s]), int(dcT.encSize[s]))
				w.writeBits(mantissa(diff, s), s)
				// AC
				run := 0
				for k := 1; k < 64; k++ {
					v := int(blk[k])
					if v == 0 {
						run++
						continue
					}
					for run > 15 {
						w.writeBits(uint32(acT.encCode[0xF0]), int(acT.encSize[0xF0])) // ZRL
						run -= 16
					}
					sz := magCat(v)
					sym := byte(run<<4 | sz)
					w.writeBits(uint32(acT.encCode[sym]), int(acT.encSize[sym]))
					w.writeBits(mantissa(v, sz), sz)
					run = 0
				}
				if run > 0 {
					w.writeBits(uint32(acT.encCode[0x00]), int(acT.encSize[0x00])) // EOB
				}
			}
		}
	}
	w.pad()
	return w.out
}

// ---- 係数平面のシリアライズ(位置ごとに束ねて zstd の効きを最大化) ----
func serializePlanes(f *jpegFrame, coeff [][]int16) []byte {
	var buf []byte
	tmp := make([]byte, binary.MaxVarintLen64)
	for ci := range f.comps {
		nBlk := len(coeff[ci]) / 64
		for k := 0; k < 64; k++ {
			for b := 0; b < nBlk; b++ {
				n := binary.PutVarint(tmp, int64(coeff[ci][b*64+k]))
				buf = append(buf, tmp[:n]...)
			}
		}
	}
	return buf
}

func deserializePlanes(f *jpegFrame, data []byte) ([][]int16, error) {
	nMCU := f.mcuX * f.mcuY
	coeff := make([][]int16, len(f.comps))
	off := 0
	for ci := range f.comps {
		nBlk := nMCU * f.comps[ci].blocksMC
		coeff[ci] = make([]int16, nBlk*64)
		for k := 0; k < 64; k++ {
			for b := 0; b < nBlk; b++ {
				v, n := binary.Varint(data[off:])
				if n <= 0 {
					return nil, errors.New("係数平面が壊れています")
				}
				off += n
				coeff[ci][b*64+k] = int16(v)
			}
		}
	}
	return coeff, nil
}

// jpegProbeEncoder は採用判定用の zstd。ストアの実経路(zstd-19/22)に
// 近い最高レベルで見積もることで、実際には縮む JPEG を「弱い probe で
// 縮まないと誤判定して不採用」にする取りこぼしを防ぐ。実測では probe が
// SpeedDefault だと実写真を軒並み取りこぼしたが、最高レベルでは
// -3.5〜-7%(スクショは -73%)で正しく採用できる。probe は保存経路の
// 実圧縮(libzstd-19)よりやや弱いので、判定は保守的(実際はより縮む)。
var jpegProbeEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))

// TryUnwrapJPEG は baseline JPEG を「ヘッダ + 係数平面」に分解する。
// maxPlain は係数平面の上限(メモリ保護)。採用は「係数平面を zstd 圧縮した
// 見込みサイズが元 JPEG 以下」の場合のみ(単画像で悪化させない。似た画像
// 同士の dedup/デルタはこれに加えてさらに効く)。
func TryUnwrapJPEG(orig []byte, maxPlain int64) (*JPEGUnwrapped, bool) {
	if maxPlain <= 0 || maxPlain > maxPlainTotal {
		maxPlain = maxPlainTotal
	}
	if len(orig) < 128 || !IsJPEG(orig) {
		return nil, false
	}
	// プログレッシブ(SOF2)は専用経路へ委譲する。
	if isProgressiveJPEG(orig) {
		return TryUnwrapJPEGProgressive(orig, maxPlain)
	}
	// 末尾の EOI(FF D9)を探す
	idxEOI := -1
	for i := len(orig) - 2; i >= 2; i-- {
		if orig[i] == 0xFF && orig[i+1] == mEOI {
			idxEOI = i
			break
		}
	}
	if idxEOI < 0 {
		return nil, false
	}
	frame, entropyStart, err := parseFrame(orig)
	if err != nil || entropyStart >= idxEOI {
		return nil, false
	}
	// 係数平面の概算上限(1係数あたり最大2バイト)で早期に諦める
	nMCU := int64(frame.mcuX) * int64(frame.mcuY)
	var totalBlocks int64
	for i := range frame.comps {
		totalBlocks += nMCU * int64(frame.comps[i].blocksMC)
	}
	if totalBlocks*64*2 > maxPlain {
		return nil, false
	}

	entropy := orig[entropyStart:idxEOI]
	coeff, err := frame.decodeScan(entropy)
	if err != nil {
		return nil, false
	}
	// ビット一致の自己検証(非標準エンコーダ産は採用しない)
	reenc := frame.encodeScan(coeff)
	if !bytes.Equal(reenc, entropy) {
		return nil, false
	}
	prefix := orig[:entropyStart]

	// 2つのコーダを見積もって小さい方を選ぶ:
	//  (a) 係数平面 varint → store の zstd-19 が圧縮(スクショ・図で強い。
	//      さらに近似重複画像の dedup/デルタが効く)
	//  (b) 文脈モデル+レンジ符号(実写真で -22〜-27%。lepton 級)
	planes := serializePlanes(frame, coeff)
	if int64(len(planes)) > maxPlain {
		return nil, false
	}
	probe := jpegProbeEncoder.EncodeAll(planes, make([]byte, 0, len(planes)/2))
	planesTotal := len(prefix) + len(probe)

	arith := frame.encodeCoefficients(coeff)
	// 安全ガード: この係数で算術符号が完全往復することを保存前に検証する
	// (エンコード/デコードの非対称バグを採用しない=読み出し不能を防ぐ)。
	arithOK := coeffEqual(coeff, frame.decodeCoefficients(arith))
	arithTotal := len(prefix) + len(arith)

	var payload []byte
	var coder int
	if arithOK && arithTotal <= planesTotal {
		payload, coder = arith, jpegCoderArith
	} else {
		payload, coder = planes, jpegCoderPlanes
	}
	// 悪化回避: 選んだ見込みサイズが元 JPEG 未満のときだけ採用。
	chosenTotal := planesTotal
	if coder == jpegCoderArith {
		chosenTotal = arithTotal
	}
	if chosenTotal >= len(orig) {
		return nil, false
	}
	chunked := make([]byte, 0, len(prefix)+len(payload))
	chunked = append(chunked, prefix...)
	chunked = append(chunked, payload...)
	return &JPEGUnwrapped{
		Chunked: chunked,
		Recipe:  &JPEGRecipe{PrefixLen: len(prefix), Suffix: append([]byte(nil), orig[idxEOI:]...), Coder: coder},
	}, true
}

func coeffEqual(a, b [][]int16) bool {
	if len(a) != len(b) {
		return false
	}
	for ci := range a {
		if len(a[ci]) != len(b[ci]) {
			return false
		}
		for i := range a[ci] {
			if a[ci][i] != b[ci][i] {
				return false
			}
		}
	}
	return true
}

// ReconstructJPEG はレシピとチャンク化内容から元の JPEG をビット単位で戻す。
func ReconstructJPEG(recipe *JPEGRecipe, chunked []byte) ([]byte, error) {
	if recipe == nil || recipe.PrefixLen < 0 || recipe.PrefixLen > len(chunked) {
		return nil, errors.New("JPEG レシピが不正です")
	}
	if recipe.Progressive {
		return ReconstructJPEGProgressive(recipe, chunked)
	}
	prefix := chunked[:recipe.PrefixLen]
	payload := chunked[recipe.PrefixLen:]
	frame, entropyStart, err := parseFrame(prefix)
	if err != nil {
		return nil, err
	}
	if entropyStart != len(prefix) {
		return nil, errors.New("JPEG prefix が SOS で終わっていません")
	}
	var coeff [][]int16
	switch recipe.Coder {
	case jpegCoderArith:
		coeff = frame.decodeCoefficients(payload)
	default:
		coeff, err = deserializePlanes(frame, payload)
		if err != nil {
			return nil, err
		}
	}
	entropy := frame.encodeScan(coeff)
	out := make([]byte, 0, len(prefix)+len(entropy)+len(recipe.Suffix))
	out = append(out, prefix...)
	out = append(out, entropy...)
	out = append(out, recipe.Suffix...)
	return out, nil
}
