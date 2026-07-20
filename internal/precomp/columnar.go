package precomp

// 列指向変換の共有コア(CSV §4.30 / JSONL §4.31 が利用)。
//
// 転置後の各列に対し、列ごとに最適なコーデックを選ぶ:
//   - raw   : 値をそのまま改行区切りで並べる(高カーディナリティ・文字列列)
//   - delta : 全データ行が正準 int の列を隣接差分にする(単調 ID・時刻列)
//   - dict  : 低カーディナリティ列を「パレット + ID列」に分ける(列挙・
//             ステータス・ホスト名などログの主成分)。転置+LZ だけより
//             エントロピー段が ID 分布を密にモデル化でき、実測でさらに縮む。
//
// 従来は「転置のみ / 数値列を一括 delta」の 2 択を全体 probe で比較していた
// (全ランダム int 列に不利な delta をかける弱点があった)。列ごとの best-of
// にすることで、ランダム列は raw、列挙列は dict、単調列は delta と正しく
// 振り分かる。実測(疑似アクセスログ CSV)で従来比 **-8.0%**、行指向比では
// -23.5% → -29.7%(RESEARCH.md §4.43)。
//
// 各列セグメントは全て '\n' 終端の行の連なりで、レシピの ColBytes(列ごとの
// セグメントバイト長)でスライスして復元する。値の中身は一切意味解釈せず、
// 位置と行数だけで往復するので可逆(採用前に必ずバイト一致検証する)。

import (
	"bytes"
	"errors"
	"strconv"
)

// 列コーデック識別子(レシピに保存)。
const (
	colRaw      uint8 = 0
	colDelta    uint8 = 1 // 正準 int の隣接差分
	colDict     uint8 = 2 // パレット + ASCII-ID(改行区切り)
	colDictBin  uint8 = 3 // パレット + 固定幅 LE 二進 ID(区切り無し)
	colDeltaDec uint8 = 4 // 固定小数(同一小数桁)のスケール整数隣接差分
)

// colDictMaxCard は dict を試すカーディナリティ上限(パレットが大きすぎると
// ID 化の利得が消える)。
const colDictMaxCard = 65536

// encodeColumns は grid(nrows×ncol)を列ごとに最適コーデックで直列化し、
// 連結した blob と、列ごとのコーデック・セグメントバイト長を返す。
func encodeColumns(grid [][][]byte, ncol, nrows int) (blob []byte, codecs []uint8, colBytes []int) {
	var out bytes.Buffer
	out.Grow(nrows * ncol * 4)
	codecs = make([]uint8, ncol)
	colBytes = make([]int, ncol)
	for c := 0; c < ncol; c++ {
		raw := colEncodeRaw(grid, c, nrows)
		best, bestCodec := raw, colRaw
		bestPL := probeLen(raw)

		if d, ok := colEncodeDelta(grid, c, nrows); ok {
			if pl := probeLen(d); pl < bestPL {
				best, bestCodec, bestPL = d, colDelta, pl
			}
		}
		// 固定小数(気温・価格・センサ値)の隣接差分。**隣接値が相関する(漸増)
		// 列だけ**を候補にする(colEncodeDeltaDec 内で相関判定)。乱数小数は差分が
		// 縮まないので候補にせず raw に退避(probeLen は zstd 概算のため、乱数列で
		// 誤って delta を選ぶのを防ぐ)。RESEARCH §4.51。
		if d, ok := colEncodeDeltaDec(grid, c, nrows); ok {
			if pl := probeLen(d); pl < bestPL {
				best, bestCodec, bestPL = d, colDeltaDec, pl
			}
		}
		// dict は ASCII-ID 版と二進-ID 版の両方を試して小さい方を採る。
		// 二進 ID(固定幅 LE、区切り無し)は列挙・ステータス等でエントロピー
		// 段が密にモデル化でき、実測で ASCII より縮む(RESEARCH §4.45)。
		if order, ids, ok := colDictBuild(grid, c, nrows); ok {
			if dv := colFormatDict(order, ids); dv != nil {
				if pl := probeLen(dv); pl < bestPL {
					best, bestCodec, bestPL = dv, colDict, pl
				}
			}
			if dvb := colFormatDictBin(order, ids); dvb != nil {
				if pl := probeLen(dvb); pl < bestPL {
					best, bestCodec, bestPL = dvb, colDictBin, pl
				}
			}
		}
		codecs[c] = bestCodec
		colBytes[c] = len(best)
		out.Write(best)
	}
	return out.Bytes(), codecs, colBytes
}

// colEncodeRaw は列 c を「各行の値 + '\n'」で直列化する。
func colEncodeRaw(grid [][][]byte, c, nrows int) []byte {
	var b bytes.Buffer
	for r := 0; r < nrows; r++ {
		b.Write(grid[r][c])
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// colEncodeDelta は列 c(データ行 1..n-1 が全て正準 int の時のみ)を
// 「row0 逐語 + 以降は隣接差分」で直列化する。row0 が非 int なら基準は 0。
func colEncodeDelta(grid [][][]byte, c, nrows int) ([]byte, bool) {
	if nrows < 2 {
		return nil, false
	}
	for r := 1; r < nrows; r++ {
		if _, ok := parseCanonInt(grid[r][c]); !ok {
			return nil, false
		}
	}
	var b bytes.Buffer
	b.Write(grid[0][c])
	b.WriteByte('\n')
	prev, _ := parseCanonInt(grid[0][c])
	for r := 1; r < nrows; r++ {
		v, _ := parseCanonInt(grid[r][c])
		b.WriteString(strconv.FormatInt(v-prev, 10))
		b.WriteByte('\n')
		prev = v
	}
	return b.Bytes(), true
}

// parseFixedDec は b が固定小数(整数部 '.' 小数部、小数桁 >=1)として正準
// 往復する(parse→format が元と一致)なら、スケール整数と小数桁を返す。
// 先頭ゼロ・'+'・"-0.00"・整数(小数点なし)は非対象=false。
func parseFixedDec(b []byte) (scaled int64, scale int, ok bool) {
	s := string(b)
	if len(s) < 3 || len(s) > 19 {
		return 0, 0, false
	}
	neg := false
	t := s
	if t[0] == '-' {
		neg = true
		t = t[1:]
	}
	dot := -1
	for i := 0; i < len(t); i++ {
		if t[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(t)-1 { // 小数点が先頭/末尾/無しは不可
		return 0, 0, false
	}
	intPart, frac := t[:dot], t[dot+1:]
	if len(intPart) > 1 && intPart[0] == '0' { // 先頭ゼロ不可
		return 0, 0, false
	}
	v, err := strconv.ParseInt(intPart+frac, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if neg {
		v = -v
	}
	sc := len(frac)
	if formatFixedDec(v, sc) != s { // 正準往復の確認("-0.00" 等を弾く)
		return 0, 0, false
	}
	return v, sc, true
}

// formatFixedDec はスケール整数と小数桁から固定小数文字列を作る。
func formatFixedDec(v int64, scale int) string {
	neg := v < 0
	u := v
	if neg {
		u = -u
	}
	str := strconv.FormatInt(u, 10)
	for len(str) <= scale { // 小数桁を満たすよう先頭ゼロ詰め
		str = "0" + str
	}
	res := str[:len(str)-scale] + "." + str[len(str)-scale:]
	if neg {
		res = "-" + res
	}
	return res
}

// colEncodeDeltaDec は固定小数列(データ行が全て同一小数桁の固定小数)を
// 「小数桁 + row0 逐語 + 以降スケール整数の隣接差分」で直列化する。気温・
// 価格・センサ値など漸増する小数列に有効(乱数小数は best-of で raw に退避)。
func colEncodeDeltaDec(grid [][][]byte, c, nrows int) ([]byte, bool) {
	if nrows < 2 {
		return nil, false
	}
	scale := -1
	scaled := make([]int64, nrows)
	for r := 1; r < nrows; r++ {
		v, sc, ok := parseFixedDec(grid[r][c])
		if !ok {
			return nil, false
		}
		if scale == -1 {
			scale = sc
		} else if sc != scale {
			return nil, false // 小数桁が揃わない列は対象外
		}
		scaled[r] = v
	}
	if scale < 0 {
		return nil, false
	}
	// 相関判定: 隣接差分の総和が値の総和より十分小さい(=漸増)列だけを
	// 対象にする。乱数小数は差分が値と同程度になり delta 化しても縮まないので
	// 候補から外す(probeLen が乱数列で誤選択するのを未然に防ぐ)。
	absF := func(x int64) float64 {
		if x < 0 {
			return float64(-x)
		}
		return float64(x)
	}
	var sumDelta, sumVal float64
	for r := 2; r < nrows; r++ {
		sumDelta += absF(scaled[r] - scaled[r-1])
	}
	for r := 1; r < nrows; r++ {
		sumVal += absF(scaled[r])
	}
	if sumDelta*2 >= sumVal { // 相関が弱い(乱数的)→ 非対象
		return nil, false
	}
	var b bytes.Buffer
	b.WriteString(strconv.Itoa(scale))
	b.WriteByte('\n')
	b.Write(grid[0][c])
	b.WriteByte('\n')
	prev := int64(0)
	if v, sc, ok := parseFixedDec(grid[0][c]); ok && sc == scale {
		prev = v
	}
	for r := 1; r < nrows; r++ {
		b.WriteString(strconv.FormatInt(scaled[r]-prev, 10))
		b.WriteByte('\n')
		prev = scaled[r]
	}
	return b.Bytes(), true
}

// colDictBuild は列 c のパレット(order)と行ごとの ID を作る。
// カーディナリティが閾値(行数の半分未満かつ colDictMaxCard 未満)に達した
// 時点で即座に打ち切る(高カーディナリティ列で無駄な O(n) スキャンと大量
// 確保を避ける)。採否は旧版と厳密に等価(最終 len(order) が閾値以上で不採用)。
func colDictBuild(grid [][][]byte, c, nrows int) (order [][]byte, ids []int, ok bool) {
	// len(order) がこの値に達したら dict 不適(len(order)*2>=nrows または
	// colDictMaxCard 到達と等価)。
	limit := (nrows + 1) / 2
	if colDictMaxCard < limit {
		limit = colDictMaxCard
	}
	if limit < 1 {
		return nil, nil, false
	}
	hint := limit
	if hint > 4096 { // 巨大な事前確保を防ぐ(以降は伸長に任せる)
		hint = 4096
	}
	seen := make(map[string]int, hint+1)
	order = make([][]byte, 0, 16)
	// ids は事前確保しない(不採用列で nrows 分を無駄に確保しないため)。
	for r := 0; r < nrows; r++ {
		s := string(grid[r][c])
		id, seenIt := seen[s]
		if !seenIt {
			id = len(order)
			seen[s] = id
			order = append(order, grid[r][c])
			if len(order) >= limit { // カーディナリティ過大 → 早期打ち切り
				return nil, nil, false
			}
		}
		ids = append(ids, id)
	}
	if len(order) == 0 {
		return nil, nil, false
	}
	return order, ids, true
}

// colFormatDict は「npal + パレット + ASCII-ID列(改行区切り)」に直列化する。
func colFormatDict(order [][]byte, ids []int) []byte {
	var b bytes.Buffer
	b.WriteString(strconv.Itoa(len(order)))
	b.WriteByte('\n')
	for _, v := range order {
		b.Write(v)
		b.WriteByte('\n')
	}
	for _, id := range ids {
		b.WriteString(strconv.Itoa(id))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// colFormatDictBin は「npal + パレット + 固定幅 LE 二進 ID列(区切り無し)」に
// 直列化する。ID 幅は npal を表すのに必要な最小バイト数(npal<=1 なら 0 幅)。
func colFormatDictBin(order [][]byte, ids []int) []byte {
	var b bytes.Buffer
	b.WriteString(strconv.Itoa(len(order)))
	b.WriteByte('\n')
	for _, v := range order {
		b.Write(v)
		b.WriteByte('\n')
	}
	w := dictIDWidth(len(order))
	for _, id := range ids {
		for k := 0; k < w; k++ {
			b.WriteByte(byte(id >> uint(8*k)))
		}
	}
	return b.Bytes()
}

// dictIDWidth は npal 個の ID を表す固定バイト幅([0,npal-1] を格納可能な最小)。
func dictIDWidth(npal int) int {
	w := 0
	for (1 << uint(8*w)) < npal {
		w++
	}
	return w
}

// decodeLegacyGrid は旧形式(ColBytes 無し)ブロブを固定行グリッド + 一括
// delta で復元する。CSV/JSONL の後方互換経路が共用する(以前は両者が
// バイト単位で同一の復号を重複実装していた)。
func decodeLegacyGrid(blob []byte, delta []bool, ncol, nrows int) ([][][]byte, error) {
	parts := bytes.Split(blob, []byte{'\n'})
	if len(parts) != ncol*nrows+1 || len(parts[len(parts)-1]) != 0 {
		return nil, errors.New("旧形式ブロブの要素数が不一致")
	}
	grid := make([][][]byte, nrows)
	for r := 0; r < nrows; r++ {
		grid[r] = make([][]byte, ncol)
	}
	for c := 0; c < ncol; c++ {
		base := c * nrows
		isDelta := delta != nil && c < len(delta) && delta[c]
		if isDelta {
			row0 := parts[base]
			grid[0][c] = row0
			// 旧形式は当時の 18桁上限で基準を再現する(19桁 row0 の既存保存物が
			// 壊れないため。詳細は parseCanonIntLegacy のコメント)。
			prev, _ := parseCanonIntLegacy(row0) // 非intなら 0
			for r := 1; r < nrows; r++ {
				d, err := strconv.ParseInt(string(parts[base+r]), 10, 64)
				if err != nil {
					return nil, errors.New("旧形式 delta の解析に失敗")
				}
				v := prev + d
				grid[r][c] = []byte(strconv.FormatInt(v, 10))
				prev = v
			}
		} else {
			for r := 0; r < nrows; r++ {
				grid[r][c] = parts[base+r]
			}
		}
	}
	return grid, nil
}

// decodeColumns は encodeColumns の逆変換。blob を colBytes で列セグメントに
// 分け、コーデックごとに grid(nrows×ncol)へ復元する。
func decodeColumns(blob []byte, codecs []uint8, colBytes []int, ncol, nrows int) ([][][]byte, error) {
	if len(codecs) != ncol || len(colBytes) != ncol {
		return nil, errors.New("列コーデック/長さの数が列数と不一致")
	}
	grid := make([][][]byte, nrows)
	for r := 0; r < nrows; r++ {
		grid[r] = make([][]byte, ncol)
	}
	off := 0
	for c := 0; c < ncol; c++ {
		// off+colBytes[c] は加算オーバフローで負に化けて検査をすり抜けうる
		// ため、残量との比較で境界を確かめる(off<=len(blob) は不変)。
		if colBytes[c] < 0 || colBytes[c] > len(blob)-off {
			return nil, errors.New("列セグメント長が範囲外")
		}
		seg := blob[off : off+colBytes[c]]
		off += colBytes[c]
		if err := colDecode(seg, codecs[c], c, nrows, grid); err != nil {
			return nil, err
		}
	}
	if off != len(blob) {
		return nil, errors.New("列セグメントの総長が blob と不一致")
	}
	return grid, nil
}

// colDecode は 1 列セグメントを grid の列 c へ復元する。
func colDecode(seg []byte, codec uint8, c, nrows int, grid [][][]byte) error {
	// 二進 ID の dict は末尾に 0x0A を含みうるので、汎用の '\n' split より前に
	// 専用復号する。
	if codec == colDictBin {
		return colDecodeDictBin(seg, c, nrows, grid)
	}
	parts := bytes.Split(seg, []byte{'\n'})
	if n := len(parts); n == 0 || len(parts[n-1]) != 0 {
		return errors.New("列セグメントが '\\n' 終端でない")
	}
	parts = parts[:len(parts)-1] // 末尾の空要素を除く
	switch codec {
	case colRaw:
		if len(parts) != nrows {
			return errors.New("raw 列の行数不一致")
		}
		for r := 0; r < nrows; r++ {
			grid[r][c] = parts[r]
		}
	case colDelta:
		if len(parts) != nrows || nrows < 1 {
			return errors.New("delta 列の行数不一致")
		}
		grid[0][c] = parts[0]
		prev, _ := parseCanonInt(parts[0])
		for r := 1; r < nrows; r++ {
			d, err := strconv.ParseInt(string(parts[r]), 10, 64)
			if err != nil {
				return errors.New("delta 値の解析に失敗")
			}
			v := prev + d
			grid[r][c] = []byte(strconv.FormatInt(v, 10))
			prev = v
		}
	case colDeltaDec:
		if len(parts) != nrows+1 { // 小数桁 + row0 + (nrows-1) 差分
			return errors.New("固定小数 delta 列の要素数不一致")
		}
		scale, err := strconv.Atoi(string(parts[0]))
		if err != nil || scale < 1 || scale > 18 {
			return errors.New("固定小数の小数桁が不正")
		}
		grid[0][c] = parts[1]
		prev := int64(0)
		if v, sc, ok := parseFixedDec(parts[1]); ok && sc == scale {
			prev = v
		}
		for r := 1; r < nrows; r++ {
			d, err := strconv.ParseInt(string(parts[r+1]), 10, 64)
			if err != nil {
				return errors.New("固定小数 delta 値の解析に失敗")
			}
			v := prev + d
			grid[r][c] = []byte(formatFixedDec(v, scale))
			prev = v
		}
	case colDict:
		if len(parts) < 1 {
			return errors.New("dict 列が空")
		}
		npal, err := strconv.Atoi(string(parts[0]))
		if err != nil || npal < 0 || npal >= colDictMaxCard {
			return errors.New("dict パレット数が不正")
		}
		if len(parts) != 1+npal+nrows {
			return errors.New("dict 列の要素数不一致")
		}
		palette := parts[1 : 1+npal]
		ids := parts[1+npal:]
		for r := 0; r < nrows; r++ {
			id, err := strconv.Atoi(string(ids[r]))
			if err != nil || id < 0 || id >= npal {
				return errors.New("dict ID が範囲外")
			}
			grid[r][c] = palette[id]
		}
	default:
		return errors.New("未知の列コーデック")
	}
	return nil
}

// colDecodeDictBin は二進 ID の dict セグメントを復元する。ヘッダ
// (npal 行 + パレット npal 行)は '\n' 区切りで読み、残りを nrows*width の
// 固定幅 LE ID 列として扱う。
func colDecodeDictBin(seg []byte, c, nrows int, grid [][][]byte) error {
	nl := bytes.IndexByte(seg, '\n')
	if nl < 0 {
		return errors.New("dictbin: npal 行がない")
	}
	npal, err := strconv.Atoi(string(seg[:nl]))
	if err != nil || npal < 1 || npal >= colDictMaxCard {
		return errors.New("dictbin: パレット数が不正")
	}
	pos := nl + 1
	palette := make([][]byte, npal)
	for i := 0; i < npal; i++ {
		j := bytes.IndexByte(seg[pos:], '\n')
		if j < 0 {
			return errors.New("dictbin: パレットが途切れた")
		}
		palette[i] = seg[pos : pos+j]
		pos += j + 1
	}
	w := dictIDWidth(npal)
	if w*nrows != len(seg)-pos { // ID 列は厳密に nrows*width バイト
		return errors.New("dictbin: ID 列の長さ不一致")
	}
	ids := seg[pos:]
	for r := 0; r < nrows; r++ {
		id := 0
		for k := 0; k < w; k++ {
			id |= int(ids[r*w+k]) << uint(8*k)
		}
		if id >= npal {
			return errors.New("dictbin: ID が範囲外")
		}
		grid[r][c] = palette[id]
	}
	return nil
}
