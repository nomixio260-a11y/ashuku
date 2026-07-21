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
	colDelta2   uint8 = 5 // 正準 int の二階差分(間隔が漸増/大ジッタの時系列)
	colHexPack  uint8 = 6 // 固定偶数幅・小文字hex を nibble パック(二進、区切り無し)
	colHexDelta uint8 = 7 // 固定幅・小文字hex の整数(uint64)隣接差分(単調hexカウンタ)
	colZPadDlt  uint8 = 8 // 固定幅・ゼロ埋め10進の整数隣接差分(ゼロ埋め連番)
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
		// 二階差分。間隔が漸増する/一階差分の振れが大きい時系列で colDelta を上回る
		// (RESEARCH §4.52)。効かない列は probeLen で選ばれないので無害。
		if d, ok := colEncodeDelta2(grid, c, nrows); ok {
			if pl := probeLen(d); pl < bestPL {
				best, bestCodec, bestPL = d, colDelta2, pl
			}
		}
		// hex ID / ゼロ埋め連番の列(トレース/リクエスト ID・ハッシュ・連番)。
		// 乱数hex は nibble パック、単調hex/ゼロ埋め10進は整数 delta(RESEARCH §4.55)。
		if d, ok := colEncodeHexPack(grid, c, nrows); ok {
			if pl := probeLen(d); pl < bestPL {
				best, bestCodec, bestPL = d, colHexPack, pl
			}
		}
		if d, ok := colEncodeHexDelta(grid, c, nrows); ok {
			if pl := probeLen(d); pl < bestPL {
				best, bestCodec, bestPL = d, colHexDelta, pl
			}
		}
		if d, ok := colEncodeZPadDelta(grid, c, nrows); ok {
			if pl := probeLen(d); pl < bestPL {
				best, bestCodec, bestPL = d, colZPadDlt, pl
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
	// 値は 1 度だけ parse する(適格性判定と符号化で二重 parse しない)。
	vals := make([]int64, nrows)
	for r := 1; r < nrows; r++ {
		v, ok := parseCanonInt(grid[r][c])
		if !ok {
			return nil, false
		}
		vals[r] = v
	}
	var b bytes.Buffer
	b.Write(grid[0][c])
	b.WriteByte('\n')
	prev := deltaBaseCanon(grid[0][c]) // 基準導出は凍結境界(parseCanonInt の変更から隔離)
	for r := 1; r < nrows; r++ {
		b.WriteString(strconv.FormatInt(vals[r]-prev, 10))
		b.WriteByte('\n')
		prev = vals[r]
	}
	return b.Bytes(), true
}

// colEncodeDelta2 は正準 int 列を「row0 逐語 + 一階差分の初項 + 以降は二階差分」で
// 直列化する。間隔が漸増(加速)する列や、一階差分の振れ幅が大きいジッタ時系列で
// colDelta を上回る(二階差分が 0 付近の小さな値に潰れて縮む)。best-of で probeLen が
// 最小のときだけ採用されるので、効かない列では選ばれず無害。整数演算のラップは
// encode/decode で対称なので、桁溢れがあっても厳密に往復する(採否は byte 一致で担保)。
func colEncodeDelta2(grid [][][]byte, c, nrows int) ([]byte, bool) {
	if nrows < 3 {
		return nil, false // 2 行以下は colDelta と等価
	}
	vals := make([]int64, nrows)
	for r := 1; r < nrows; r++ {
		v, ok := parseCanonInt(grid[r][c])
		if !ok {
			return nil, false
		}
		vals[r] = v
	}
	vals[0] = deltaBaseCanon(grid[0][c]) // 基準は凍結境界(colDelta と共通)
	var b bytes.Buffer
	b.Write(grid[0][c])
	b.WriteByte('\n')
	prevDiff := vals[1] - vals[0] // 一階差分の初項
	b.WriteString(strconv.FormatInt(prevDiff, 10))
	b.WriteByte('\n')
	for r := 2; r < nrows; r++ {
		diff := vals[r] - vals[r-1]
		b.WriteString(strconv.FormatInt(diff-prevDiff, 10)) // 二階差分
		b.WriteByte('\n')
		prevDiff = diff
	}
	return b.Bytes(), true
}

// isLowerHex は b が小文字16進([0-9a-f])のみからなるか。
func isLowerHex(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func hexNibble(c byte) byte {
	if c <= '9' {
		return c - '0'
	}
	return c - 'a' + 10
}

const hexDigits = "0123456789abcdef"

// padLeft は s を '0' 詰めで幅 w に左詰めする。幅超過はそのまま返す(byte 一致
// ゲートが弾く)。
func padLeft(s string, w int) []byte {
	if len(s) >= w {
		return []byte(s)
	}
	out := make([]byte, w)
	pad := w - len(s)
	for i := 0; i < pad; i++ {
		out[i] = '0'
	}
	copy(out[pad:], s)
	return out
}

// colEncodeHexPack は列のデータ行が全て同一の偶数幅・小文字hex(トレースID・
// リクエストID・ハッシュ等)なら nibble パックして二進化する。ASCII 8bit/文字を
// 4bit/文字に詰め、zstd が乱数hexで届かない 4bit エントロピー床まで縮める。
func colEncodeHexPack(grid [][][]byte, c, nrows int) ([]byte, bool) {
	if nrows < 2 {
		return nil, false
	}
	w := len(grid[1][c])
	if w < 2 || w%2 != 0 || w > 4096 {
		return nil, false
	}
	for r := 1; r < nrows; r++ {
		if len(grid[r][c]) != w || !isLowerHex(grid[r][c]) {
			return nil, false
		}
	}
	var b bytes.Buffer
	b.Write(grid[0][c]) // row0(ヘッダ)は逐語
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(w))
	b.WriteByte('\n')
	tmp := make([]byte, w/2)
	for r := 1; r < nrows; r++ {
		v := grid[r][c]
		for i := 0; i < w/2; i++ {
			tmp[i] = hexNibble(v[2*i])<<4 | hexNibble(v[2*i+1])
		}
		b.Write(tmp)
	}
	return b.Bytes(), true
}

// colDecodeHexPack は colEncodeHexPack の逆。区切り無しの二進なので汎用 split の
// 前に専用復号する(colDictBin と同じ扱い)。
func colDecodeHexPack(seg []byte, c, nrows int, grid [][][]byte) error {
	nl := bytes.IndexByte(seg, '\n')
	if nl < 0 {
		return errors.New("hexpack: row0 行がない")
	}
	grid[0][c] = seg[:nl]
	rest := seg[nl+1:]
	nl2 := bytes.IndexByte(rest, '\n')
	if nl2 < 0 {
		return errors.New("hexpack: 幅行がない")
	}
	w, err := strconv.Atoi(string(rest[:nl2]))
	if err != nil || w < 2 || w%2 != 0 || w > 4096 {
		return errors.New("hexpack: 幅が不正")
	}
	packed := rest[nl2+1:]
	if len(packed) != (nrows-1)*(w/2) {
		return errors.New("hexpack: パック長不一致")
	}
	pos := 0
	for r := 1; r < nrows; r++ {
		out := make([]byte, w)
		for i := 0; i < w/2; i++ {
			bb := packed[pos+i]
			out[2*i] = hexDigits[bb>>4]
			out[2*i+1] = hexDigits[bb&0x0f]
		}
		pos += w / 2
		grid[r][c] = out
	}
	return nil
}

// colEncodeHexDelta は固定幅・小文字hex を uint64 として隣接差分にする(単調な
// hex カウンタ・連番トークン)。幅は 16(=64bit)まで。
func colEncodeHexDelta(grid [][][]byte, c, nrows int) ([]byte, bool) {
	if nrows < 2 {
		return nil, false
	}
	w := len(grid[1][c])
	if w < 1 || w > 16 {
		return nil, false
	}
	vals := make([]int64, nrows)
	for r := 1; r < nrows; r++ {
		g := grid[r][c]
		if len(g) != w || !isLowerHex(g) {
			return nil, false
		}
		v, err := strconv.ParseUint(string(g), 16, 64)
		if err != nil {
			return nil, false
		}
		vals[r] = int64(v) // 環演算で対称(復号で uint64 に戻す)
	}
	var b bytes.Buffer
	b.Write(grid[0][c])
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(w))
	b.WriteByte('\n')
	prev := int64(0)
	for r := 1; r < nrows; r++ {
		b.WriteString(strconv.FormatInt(vals[r]-prev, 10))
		b.WriteByte('\n')
		prev = vals[r]
	}
	return b.Bytes(), true
}

// colEncodeZPadDelta は固定幅・ゼロ埋め10進(先頭ゼロで parseCanonInt に弾かれる
// 連番)を整数化して隣接差分にする。幅は 19(=int64)まで。
func colEncodeZPadDelta(grid [][][]byte, c, nrows int) ([]byte, bool) {
	if nrows < 2 {
		return nil, false
	}
	w := len(grid[1][c])
	if w < 1 || w > 19 {
		return nil, false
	}
	vals := make([]int64, nrows)
	for r := 1; r < nrows; r++ {
		g := grid[r][c]
		if len(g) != w {
			return nil, false
		}
		for _, ch := range g {
			if ch < '0' || ch > '9' {
				return nil, false
			}
		}
		v, err := strconv.ParseInt(string(g), 10, 64)
		if err != nil {
			return nil, false
		}
		vals[r] = v
	}
	var b bytes.Buffer
	b.Write(grid[0][c])
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(w))
	b.WriteByte('\n')
	prev := int64(0)
	for r := 1; r < nrows; r++ {
		b.WriteString(strconv.FormatInt(vals[r]-prev, 10))
		b.WriteByte('\n')
		prev = vals[r]
	}
	return b.Bytes(), true
}

// parseFixedDec は b が固定小数(整数部 '.' 小数部、小数桁 >=1)として正準
// 往復する(parse→format が元と一致)なら、スケール整数と小数桁を返す。
// 先頭ゼロ・'+'・"-0.00"・整数(小数点なし)は非対象=false。
//
// **正準化の意味論を変える場合は要注意(凍結境界)。** 本関数は列の適格性判定に
// 加えて colDeltaDec の delta 基準 row0 の導出(colEncodeDeltaDec と colDecode の
// 両方)にも使われる。基準は保存済みレシピの復元に用いられるため、受理条件を
// 緩める/厳しくすると、当時と異なる基準で復号して保存物が読めなくなる(=データ
// 損失。parseCanonInt を 18→19桁に広げて旧 delta レシピを壊したのと同型)。
// 挙動を変えたい時は新しいコーデックバージョンを追加し、既存経路の基準導出は
// 現行の意味論のまま保つこと。
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
	// 桁部分は FormatInt(v) から符号を外して得る。u=-v による絶対値化は
	// v==math.MinInt64 でオーバフローして負のまま残り、"--..." の不正文字列に
	// なる(FormatInt は MinInt64 も正しく "-9223..." にする)。
	str := strconv.FormatInt(v, 10)
	neg := len(str) > 0 && str[0] == '-'
	if neg {
		str = str[1:]
	}
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
	// 過大確保の防御(recover では捕捉できない OOM を未然に防ぐ):
	// grid は make([][][]byte, nrows) を recipe の nrows/ncol だけで先に確保する
	// ため、破損・改竄で巨大な Rows/Cols が来ると小さな blob からでも数十GB を
	// 確保して落ちる。blob 長では縛れない(dictBin の定数列は npal=1 で ID 幅 0 と
	// なり、行数に依らず数バイトに圧縮されるため)。代わりに原文サイズ不変量で縛る:
	// 元テキストは 1 行あたり最低 ncol バイト(区切り+改行)を要し、precomp 対象の
	// 原文は必ず maxPlainTotal 以下なので、有効な recipe では nrows*ncol <=
	// maxPlainTotal。これを超える寸法は破損とみなし確保前に弾く。
	// 個別上限を先に課すことで、以降の nrows*ncol 乗算が int64 で溢れないことも保証する。
	if nrows < 0 || ncol < 0 || nrows > maxPlainTotal || ncol > maxPlainTotal ||
		int64(nrows)*int64(ncol) > maxPlainTotal {
		return nil, errors.New("列グリッド寸法が原文上限に対して過大")
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
	// 二進 ID の dict と nibble パック hex は末尾に 0x0A を含みうるので、汎用の
	// '\n' split より前に専用復号する。
	if codec == colDictBin {
		return colDecodeDictBin(seg, c, nrows, grid)
	}
	if codec == colHexPack {
		return colDecodeHexPack(seg, c, nrows, grid)
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
		prev := deltaBaseCanon(parts[0]) // encode と同一の凍結境界で基準を再現
		for r := 1; r < nrows; r++ {
			d, err := strconv.ParseInt(string(parts[r]), 10, 64)
			if err != nil {
				return errors.New("delta 値の解析に失敗")
			}
			v := prev + d
			grid[r][c] = []byte(strconv.FormatInt(v, 10))
			prev = v
		}
	case colDelta2:
		if len(parts) != nrows || nrows < 3 {
			return errors.New("delta2 列の行数不一致")
		}
		grid[0][c] = parts[0]
		base := deltaBaseCanon(parts[0])
		d1, err := strconv.ParseInt(string(parts[1]), 10, 64) // 一階差分の初項
		if err != nil {
			return errors.New("delta2 初項の解析に失敗")
		}
		prevVal := base + d1 // vals[1]
		grid[1][c] = []byte(strconv.FormatInt(prevVal, 10))
		prevDiff := d1
		for r := 2; r < nrows; r++ {
			dd, err := strconv.ParseInt(string(parts[r]), 10, 64) // 二階差分
			if err != nil {
				return errors.New("delta2 二階差分の解析に失敗")
			}
			prevDiff += dd
			prevVal += prevDiff
			grid[r][c] = []byte(strconv.FormatInt(prevVal, 10))
		}
	case colHexDelta:
		if len(parts) != nrows+1 { // row0 + 幅 + (nrows-1) 差分
			return errors.New("hexdelta 列の要素数不一致")
		}
		grid[0][c] = parts[0]
		w, err := strconv.Atoi(string(parts[1]))
		if err != nil || w < 1 || w > 16 {
			return errors.New("hexdelta 幅が不正")
		}
		prev := int64(0)
		for r := 1; r < nrows; r++ {
			d, err := strconv.ParseInt(string(parts[r+1]), 10, 64)
			if err != nil {
				return errors.New("hexdelta 値の解析に失敗")
			}
			prev += d
			grid[r][c] = padLeft(strconv.FormatUint(uint64(prev), 16), w)
		}
	case colZPadDlt:
		if len(parts) != nrows+1 { // row0 + 幅 + (nrows-1) 差分
			return errors.New("zpad 列の要素数不一致")
		}
		grid[0][c] = parts[0]
		w, err := strconv.Atoi(string(parts[1]))
		if err != nil || w < 1 || w > 19 {
			return errors.New("zpad 幅が不正")
		}
		prev := int64(0)
		for r := 1; r < nrows; r++ {
			d, err := strconv.ParseInt(string(parts[r+1]), 10, 64)
			if err != nil {
				return errors.New("zpad 値の解析に失敗")
			}
			prev += d
			grid[r][c] = padLeft(strconv.FormatInt(prev, 10), w)
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
