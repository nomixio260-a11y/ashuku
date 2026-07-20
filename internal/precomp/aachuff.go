package precomp

// AAC ハフマン符号のデコード補助。正準符号(FFmpeg=規格の code/bits)から
// シンボルを引くための逆引きを遅延構築する。エンコードは
// aacSpecTables[cb].Codes/Bits と aacScfCodes/Bits を直接引く。

type aacDecMap struct {
	m       map[uint32]int // key = (nbits<<20)|code → symbol
	maxBits int
}

func buildAACDecMap(codes []uint32, bits []uint8) *aacDecMap {
	d := &aacDecMap{m: make(map[uint32]int, len(codes))}
	for i := range codes {
		nb := int(bits[i])
		if nb == 0 {
			continue
		}
		if nb > d.maxBits {
			d.maxBits = nb
		}
		d.m[uint32(nb)<<20|codes[i]] = i
	}
	return d
}

var aacSpecDec [12]*aacDecMap
var aacScfDec *aacDecMap

func aacInitDec() {
	if aacScfDec != nil {
		return
	}
	for cb := 1; cb <= 11; cb++ {
		t := &aacSpecTables[cb]
		u32 := make([]uint32, len(t.Codes))
		for i, c := range t.Codes {
			u32[i] = uint32(c)
		}
		aacSpecDec[cb] = buildAACDecMap(u32, t.Bits)
	}
	aacScfDec = buildAACDecMap(aacScfCodes[:], aacScfBits[:])
}

// aacDecodeSym は逆引きマップで 1 シンボルを復号する。
func (d *aacDecMap) decode(r *h264Reader) (int, bool) {
	code := uint32(0)
	for nb := 1; nb <= d.maxBits; nb++ {
		b, err := r.u1()
		if err != nil {
			return 0, false
		}
		code = code<<1 | b
		if sym, ok := d.m[uint32(nb)<<20|code]; ok {
			return sym, true
		}
	}
	return 0, false
}

// aacSpecMags は spectral シンボルを Dim 個の桁(magnitude/signed 値)へ分解する。
// 戻り値は Dim 要素。unsigned は magnitude(0..LAV)、signed は値+LAV(0..2LAV)。
func aacSpecMags(cb, sym int) []int {
	t := &aacSpecTables[cb]
	base := t.LAV + 1
	if !t.Unsigned {
		base = 2*t.LAV + 1
	}
	out := make([]int, t.Dim)
	for i := t.Dim - 1; i >= 0; i-- {
		out[i] = sym % base
		sym /= base
	}
	return out
}
