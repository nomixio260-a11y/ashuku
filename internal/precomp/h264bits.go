package precomp

// H.264 ビットストリーム基盤: NAL 分割・エミュレーション防止・Exp-Golomb。
//
// 方針(JPEG 再圧縮 §4.21 と同じ構図): CAVLC(ハフマン系 VLC)で符号化された
// H.264 の構文要素を**ピクセルを復号せずに**解析し、文脈適応算術符号
// (rangecoder.go)で再符号化する。読み出し時は逆に CAVLC を決定的に再生成
// してビット一致の元ストリームに戻す。IDCT・動き補償・デブロッキングは
// 一切不要(エントロピー層だけの変換)なので実装面積が桁で小さい。

import "errors"

// ---- NAL 分割(Annex B) ----

// nalUnit は Annex B ストリーム中の1 NAL。
type nalUnit struct {
	startLen int    // スタートコード長(3 or 4)
	data     []byte // スタートコード直後〜次のスタートコード直前(EBSP)
}

// splitAnnexB は 00 00 01 / 00 00 00 01 区切りで NAL を列挙する。
func splitAnnexB(b []byte) ([]nalUnit, bool) {
	var out []nalUnit
	i := 0
	// 先頭のスタートコードを探す(先頭がスタートコードでなければ非対応)
	first := findStartCode(b, 0)
	if first != 0 && first != -1 {
		return nil, false
	}
	if first == -1 {
		return nil, false
	}
	i = first
	for i < len(b) {
		sl := startCodeLen(b[i:])
		if sl == 0 {
			return nil, false
		}
		next := findStartCode(b, i+sl)
		end := len(b)
		if next >= 0 {
			end = next
		}
		out = append(out, nalUnit{startLen: sl, data: b[i+sl : end]})
		if next < 0 {
			break
		}
		i = next
	}
	return out, len(out) > 0
}

func startCodeLen(b []byte) int {
	if len(b) >= 4 && b[0] == 0 && b[1] == 0 && b[2] == 0 && b[3] == 1 {
		return 4
	}
	if len(b) >= 3 && b[0] == 0 && b[1] == 0 && b[2] == 1 {
		return 3
	}
	return 0
}

// findStartCode は位置 from 以降の次のスタートコード先頭を返す(-1=なし)。
func findStartCode(b []byte, from int) int {
	for i := from; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				return i
			}
			if i+4 <= len(b) && b[i+2] == 0 && b[i+3] == 1 {
				return i
			}
		}
	}
	return -1
}

// unescapeRBSP は EBSP → RBSP(00 00 03 → 00 00)。
func unescapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if i+2 < len(b) && b[i] == 0 && b[i+1] == 0 && b[i+2] == 3 {
			out = append(out, 0, 0)
			i += 2
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// escapeRBSP は RBSP → EBSP(00 00 {00,01,02,03} の直前に 03 挿入)。
func escapeRBSP(b []byte) []byte {
	out := make([]byte, 0, len(b)+8)
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c <= 3 {
			out = append(out, 3)
			zeros = 0
		}
		out = append(out, c)
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return out
}

// ---- MSB ファーストのビットリーダ/ライタ ----

var errH264Bits = errors.New("h264: ビットストリームが短い/不正")

type h264Reader struct {
	b   []byte
	pos int // ビット位置
}

func (r *h264Reader) bitsLeft() int { return len(r.b)*8 - r.pos }

func (r *h264Reader) u1() (uint32, error) {
	if r.pos >= len(r.b)*8 {
		return 0, errH264Bits
	}
	v := uint32(r.b[r.pos>>3]>>(7-uint(r.pos&7))) & 1
	r.pos++
	return v, nil
}

func (r *h264Reader) u(n int) (uint32, error) {
	var v uint32
	for i := 0; i < n; i++ {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// ue は Exp-Golomb 無符号。
func (r *h264Reader) ue() (uint32, error) {
	zeros := 0
	for {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		if b == 1 {
			break
		}
		zeros++
		if zeros > 31 {
			return 0, errH264Bits
		}
	}
	if zeros == 0 {
		return 0, nil
	}
	suffix, err := r.u(zeros)
	if err != nil {
		return 0, err
	}
	return (1<<uint(zeros) - 1) + suffix, nil
}

// se は Exp-Golomb 符号付き。
func (r *h264Reader) se() (int32, error) {
	k, err := r.ue()
	if err != nil {
		return 0, err
	}
	if k%2 == 0 {
		return -int32(k / 2), nil
	}
	return int32(k/2) + 1, nil
}

// te は truncated Exp-Golomb(range==1 なら 1ビット反転)。
func (r *h264Reader) te(rangeMax int) (uint32, error) {
	if rangeMax == 1 {
		b, err := r.u1()
		if err != nil {
			return 0, err
		}
		return 1 - b, nil
	}
	return r.ue()
}

// moreRBSPData は 9.x: 残りに stop bit 以降のデータがあるか。
func (r *h264Reader) moreRBSPData() bool {
	// 末尾から最後の 1(stop bit)を探し、現在位置がそれ未満なら真
	last := len(r.b)*8 - 1
	for last >= 0 {
		bit := (r.b[last>>3] >> (7 - uint(last&7))) & 1
		if bit == 1 {
			break
		}
		last--
	}
	return r.pos < last
}

type h264Writer struct {
	b    []byte
	nbit int // 総ビット数
}

func (w *h264Writer) u1(v uint32) {
	if w.nbit&7 == 0 {
		w.b = append(w.b, 0)
	}
	if v != 0 {
		w.b[w.nbit>>3] |= 1 << (7 - uint(w.nbit&7))
	}
	w.nbit++
}

func (w *h264Writer) u(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.u1((v >> uint(i)) & 1)
	}
}

func (w *h264Writer) ue(v uint32) {
	vv := v + 1
	n := 0
	for t := vv; t > 1; t >>= 1 {
		n++
	}
	for i := 0; i < n; i++ {
		w.u1(0)
	}
	w.u(vv, n+1)
}

func (w *h264Writer) se(v int32) {
	if v <= 0 {
		w.ue(uint32(-v) * 2)
	} else {
		w.ue(uint32(v)*2 - 1)
	}
}

func (w *h264Writer) te(v uint32, rangeMax int) {
	if rangeMax == 1 {
		w.u1(1 - v)
	} else {
		w.ue(v)
	}
}

// copyBits は r の現在位置から n ビットを w へ転写する。
func copyBits(w *h264Writer, r *h264Reader, n int) error {
	for i := 0; i < n; i++ {
		b, err := r.u1()
		if err != nil {
			return err
		}
		w.u1(b)
	}
	return nil
}
