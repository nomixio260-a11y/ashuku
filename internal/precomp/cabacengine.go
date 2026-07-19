package precomp

// CABAC 算術符号エンジン(ITU-T H.264 9.3)。
//
// 復号器(9.3.3.2)と符号化器(9.3.4)の両方を仕様のフローチャート通りに
// 実装する。符号化器は規格の正規手続きそのもの(x264/ハードウェアエンコーダ
// も同一)なので、同じビン列と文脈状態列を与えればビット単位に同じ出力を
// 生成する——これが CABAC ストリームの可逆再圧縮(ビン列を取り出して
// 12bit 適応確率で再符号化し、読み出し時に CABAC を決定的に再生成)の土台。
// 状態は FFmpeg と同じ 7bit 表現(s7 = 2σ + valMPS)で持ち、LPS レンジ・
// 状態遷移は機械生成テーブル(cabactables_gen.go)を引く。

// cabacDecoder は 9.3.3.2 の算術復号器。
type cabacDecoder struct {
	r      *h264Reader
	rng    uint32 // codIRange
	offset uint32 // codIOffset
	err    bool
}

func newCabacDecoder(r *h264Reader) *cabacDecoder {
	d := &cabacDecoder{r: r, rng: 510}
	v, err := r.u(9)
	if err != nil {
		d.err = true
	}
	d.offset = v
	return d
}

func (d *cabacDecoder) readBit() uint32 {
	b, err := d.r.u1()
	if err != nil {
		// スライス末尾を越えた読みは 0 詰め(スペック上は起きないが、
		// 壊れた入力で安全に進めて最終検証で弾く)
		d.err = true
		return 0
	}
	return b
}

// decodeDecision は文脈付き1ビン(9.3.3.2.1)。state は 7bit 状態への参照。
func (d *cabacDecoder) decodeDecision(state *uint8) int {
	s := *state
	lps := uint32(cabacLPSRange[((d.rng>>6)&3)<<7|uint32(s)])
	d.rng -= lps
	var bin int
	if d.offset >= d.rng {
		bin = int(1 - s&1)
		d.offset -= d.rng
		d.rng = lps
		*state = cabacMLPS[127-int(s)]
	} else {
		bin = int(s & 1)
		*state = cabacMLPS[128+int(s)]
	}
	for d.rng < 256 {
		d.rng <<= 1
		d.offset = d.offset<<1 | d.readBit()
	}
	return bin
}

// decodeBypass は等確率1ビン(9.3.3.2.3)。
func (d *cabacDecoder) decodeBypass() int {
	d.offset = d.offset<<1 | d.readBit()
	if d.offset >= d.rng {
		d.offset -= d.rng
		return 1
	}
	return 0
}

// reinitAt は I_PCM 後などにバイト境界 bitpos から算術復号器を再初期化する
// (9.3.1.2: codIRange=510, codIOffset=read_bits(9))。
func (d *cabacDecoder) reinitAt(bitpos int) {
	d.r.pos = bitpos
	d.rng = 510
	v, err := d.r.u(9)
	if err != nil {
		d.err = true
	}
	d.offset = v
}

// decodeTerminate は end_of_slice / I_PCM 用の終端ビン(9.3.3.2.4)。
func (d *cabacDecoder) decodeTerminate() int {
	d.rng -= 2
	if d.offset >= d.rng {
		return 1
	}
	for d.rng < 256 {
		d.rng <<= 1
		d.offset = d.offset<<1 | d.readBit()
	}
	return 0
}

// cabacEncoder は 9.3.4 の算術符号化器(規格の正規手続き)。
type cabacEncoder struct {
	w           *h264Writer
	low         uint32
	rng         uint32
	outstanding int
	firstBit    bool
}

func newCabacEncoder(w *h264Writer) *cabacEncoder {
	return &cabacEncoder{w: w, rng: 510, firstBit: true}
}

func (e *cabacEncoder) putBit(b uint32) {
	if e.firstBit {
		e.firstBit = false
	} else {
		e.w.u1(b)
	}
	for e.outstanding > 0 {
		e.w.u1(1 - b)
		e.outstanding--
	}
}

func (e *cabacEncoder) renorm() {
	for e.rng < 256 {
		if e.low < 256 {
			e.putBit(0)
		} else if e.low >= 512 {
			e.putBit(1)
			e.low -= 512
		} else {
			e.outstanding++
			e.low -= 256
		}
		e.low <<= 1
		e.rng <<= 1
	}
}

// encodeDecision は decodeDecision の逆(9.3.4.3.2)。
func (e *cabacEncoder) encodeDecision(state *uint8, bin int) {
	s := *state
	lps := uint32(cabacLPSRange[((e.rng>>6)&3)<<7|uint32(s)])
	e.rng -= lps
	if bin != int(s&1) {
		e.low += e.rng
		e.rng = lps
		*state = cabacMLPS[127-int(s)]
	} else {
		*state = cabacMLPS[128+int(s)]
	}
	e.renorm()
}

func (e *cabacEncoder) encodeBypass(bin int) {
	e.low <<= 1
	if bin != 0 {
		e.low += e.rng
	}
	if e.low >= 1024 {
		e.putBit(1)
		e.low -= 1024
	} else if e.low < 512 {
		e.putBit(0)
	} else {
		e.outstanding++
		e.low -= 512
	}
}

func (e *cabacEncoder) encodeTerminate(bin int) {
	e.rng -= 2
	if bin != 0 {
		e.low += e.rng
		e.flush()
	} else {
		e.renorm()
	}
}

// flush は 9.3.4.3.5: 最後の terminate=1 の後に呼ばれ、rbsp_stop_one_bit を
// 含む終端ビットを書く(この後、呼び出し側がバイト境界まで 0 詰めする)。
func (e *cabacEncoder) flush() {
	e.rng = 2
	e.renorm()
	e.putBit((e.low >> 9) & 1)
	// WriteBits(((low>>7)&3)|1, 2)
	v := ((e.low >> 7) & 3) | 1
	e.w.u1(v >> 1)
	e.w.u1(v & 1)
}

// cabacInitStates はスライス開始時の全文脈初期化(9.3.1.1)。
func cabacInitStates(states *[1024]uint8, sliceQPY int, isI bool, initIdc int) {
	var tab *[2048]int8
	if isI {
		tab = &cabacCtxInitI
	} else {
		tab = &cabacCtxInitPB[initIdc]
	}
	qp := sliceQPY
	if qp < 0 {
		qp = 0
	}
	if qp > 51 {
		qp = 51
	}
	for c := 0; c < 1024; c++ {
		m := int(tab[c*2])
		n := int(tab[c*2+1])
		pre := (m*qp)>>4 + n
		if pre < 1 {
			pre = 1
		}
		if pre > 126 {
			pre = 126
		}
		if pre <= 63 {
			states[c] = uint8((63 - pre) << 1) // valMPS=0
		} else {
			states[c] = uint8((pre-64)<<1 | 1) // valMPS=1
		}
	}
}
