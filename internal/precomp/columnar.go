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
	colRaw   uint8 = 0
	colDelta uint8 = 1
	colDict  uint8 = 2
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
		if dv, ok := colEncodeDict(grid, c, nrows); ok {
			if pl := probeLen(dv); pl < bestPL {
				best, bestCodec, bestPL = dv, colDict, pl
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

// colEncodeDict は列 c を「npal + パレット + ID列」で直列化する
// (カーディナリティが行数の半分未満かつ上限内の時のみ)。
//
// カーディナリティが閾値に達した時点で即座に打ち切る(全行を走査してから
// 判定していた旧版は、高カーディナリティ列で無駄な O(n) スキャンと、
// nrows/2 まで膨らむ seen マップの大量確保を招いた)。採否の判定は
// 旧版と厳密に等価(最終 len(order) が閾値以上なら不採用)。
func colEncodeDict(grid [][][]byte, c, nrows int) ([]byte, bool) {
	// len(order) がこの値に達したら dict 不適(len(order)*2>=nrows または
	// colDictMaxCard 到達と等価)。
	limit := (nrows + 1) / 2
	if colDictMaxCard < limit {
		limit = colDictMaxCard
	}
	if limit < 1 {
		return nil, false
	}
	hint := limit
	if hint > 4096 { // 巨大な事前確保を防ぐ(以降は伸長に任せる)
		hint = 4096
	}
	seen := make(map[string]int, hint+1)
	order := make([][]byte, 0, 16)
	for r := 0; r < nrows; r++ {
		s := string(grid[r][c])
		if _, ok := seen[s]; !ok {
			seen[s] = len(order)
			order = append(order, grid[r][c])
			if len(order) >= limit { // カーディナリティ過大 → 早期打ち切り
				return nil, false
			}
		}
	}
	if len(order) == 0 {
		return nil, false
	}
	var b bytes.Buffer
	b.WriteString(strconv.Itoa(len(order)))
	b.WriteByte('\n')
	for _, v := range order {
		b.Write(v)
		b.WriteByte('\n')
	}
	for r := 0; r < nrows; r++ {
		b.WriteString(strconv.Itoa(seen[string(grid[r][c])]))
		b.WriteByte('\n')
	}
	return b.Bytes(), true
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
