package precomp

// プログレッシブ JPEG(SOF2)の可逆分解。
//
// Web・スマホ写真の相当割合(mozjpeg/PIL の既定)がプログレッシブで、
// baseline 用の文脈算術コーダ(§4.18-19、実写真 -29〜-34%)は係数さえ
// 取り出せればそのまま効く。プログレッシブは複数スキャンで係数を段階的に
// 送る(DC/AC × 逐次近似)ため、全スキャンを復号して完全な係数配列に
// 復元し、算術符号化する。
//
// 可逆性: 復元した係数からプログレッシブスキャンを再符号化し、元の
// エントロピーバイトと**ビット一致**する場合だけ採用する(libjpeg/mozjpeg/
// PIL は標準アルゴリズムなので一致する)。一致しなければ素通し。復元は
// SHA-256 最終検証つき(store 層)。純Go・cgo 不要。

import (
	"encoding/binary"
	"errors"
)

const mSOF2 = 0xC2

// isProgressiveJPEG は最初のフレームヘッダが SOF2(プログレッシブ)かを
// 判定する(マーカを軽く走査する)。
func isProgressiveJPEG(orig []byte) bool {
	if len(orig) < 4 || orig[0] != 0xFF || orig[1] != mSOI {
		return false
	}
	i := 2
	for i+3 < len(orig) {
		if orig[i] != 0xFF {
			return false
		}
		m := orig[i+1]
		if m == mSOI || (m >= mRST0 && m <= mRST7) {
			i += 2
			continue
		}
		// SOF マーカ群(0xC0..0xCF、DHT=0xC4/JPG=0xC8/DAC=0xCC を除く)
		if m == mSOF2 {
			return true
		}
		if m == mSOF0 || m == mDHT || m == mSOS || m == mEOI {
			return false // baseline SOF0 か、SOF より先に本体
		}
		if i+4 > len(orig) {
			return false
		}
		segLen := int(binary.BigEndian.Uint16(orig[i+2:]))
		if segLen < 2 {
			return false
		}
		i += 2 + segLen
	}
	return false
}

// progScan は1スキャンの構成。
type progScan struct {
	comps    []scanComp // スキャン成分(1=非インターリーブ)
	ss, se   int
	ah, al   int
	dc, ac   [4]*huffTable // このスキャン時点の有効テーブル
	skelEnd  int           // スケルトン内でこのスキャンのエントロピーを挿入する位置
	restart  int           // このスキャン適用時の DRI
	entStart int           // 元データ内のエントロピー開始
	entEnd   int           // 元データ内のエントロピー終了(次マーカ直前)
}

type scanComp struct {
	ci     int // frame.comps のインデックス
	td, ta int
}

// progFrame はプログレッシブフレーム全体。
type progFrame struct {
	frame    *jpegFrame
	scans    []progScan
	skeleton []byte // 全エントロピーを除いた原文(ヘッダ列+EOI)
	// blocksW/blocksH は成分ごとの非インターリーブ実ブロック数。
	blocksW, blocksH []int
}

// parseProgressive はプログレッシブ JPEG を解析し、スキャン構成と
// スケルトンを返す。
func parseProgressive(orig []byte) (*progFrame, error) {
	if len(orig) < 4 || orig[0] != 0xFF || orig[1] != mSOI {
		return nil, errors.New("SOI がありません")
	}
	f := &jpegFrame{}
	pf := &progFrame{frame: f}
	var skel []byte
	skelSrc := 0 // orig 内で skeleton に未コピーの開始位置
	i := 2
	sawSOF := false
	for i+1 < len(orig) {
		if orig[i] != 0xFF {
			return nil, errors.New("マーカが不正")
		}
		m := orig[i+1]
		if m == mEOI {
			i += 2
			break
		}
		if m == mSOI || (m >= mRST0 && m <= mRST7) {
			i += 2
			continue
		}
		if i+4 > len(orig) {
			return nil, errors.New("セグメント長が不正")
		}
		segLen := int(binary.BigEndian.Uint16(orig[i+2:]))
		if segLen < 2 || i+2+segLen > len(orig) {
			return nil, errors.New("セグメント長が範囲外")
		}
		seg := orig[i+4 : i+2+segLen]
		switch m {
		case mSOF2:
			if err := parseSOFInto(f, seg); err != nil {
				return nil, err
			}
			sawSOF = true
			f.mcuX = (f.width + 8*f.hmax - 1) / (8 * f.hmax)
			f.mcuY = (f.height + 8*f.vmax - 1) / (8 * f.vmax)
			pf.blocksW = make([]int, len(f.comps))
			pf.blocksH = make([]int, len(f.comps))
			for ci := range f.comps {
				c := &f.comps[ci]
				pf.blocksW[ci] = (f.width*c.h + 8*f.hmax - 1) / (8 * f.hmax)
				pf.blocksH[ci] = (f.height*c.v + 8*f.vmax - 1) / (8 * f.vmax)
			}
			i += 2 + segLen
		case mDHT:
			if err := parseDHTInto(f, seg); err != nil {
				return nil, err
			}
			i += 2 + segLen
		case mDRI:
			if len(seg) >= 2 {
				f.restart = int(binary.BigEndian.Uint16(seg))
			}
			i += 2 + segLen
		case mSOS:
			if !sawSOF {
				return nil, errors.New("SOF より前に SOS")
			}
			sc, err := parseSOSHeader(f, seg)
			if err != nil {
				return nil, err
			}
			// SOS ヘッダまでを skeleton にコピー(エントロピーは除外)
			hdrEnd := i + 2 + segLen
			skel = append(skel, orig[skelSrc:hdrEnd]...)
			sc.skelEnd = len(skel)
			sc.restart = f.restart
			// エントロピー範囲を求める(次のマーカ直前まで。RST は内部)
			entStart := hdrEnd
			p := entStart
			for p+1 < len(orig) {
				if orig[p] == 0xFF {
					nb := orig[p+1]
					if nb == 0x00 || (nb >= mRST0 && nb <= mRST7) {
						p += 2
						continue
					}
					break
				}
				p++
			}
			if p+1 >= len(orig) {
				p = len(orig)
			}
			sc.entStart = entStart
			sc.entEnd = p
			pf.scans = append(pf.scans, sc)
			skelSrc = p // エントロピーは skeleton から除外
			i = p
		default:
			i += 2 + segLen
		}
	}
	// 残り(最後のエントロピー後の EOI 等)を skeleton に
	skel = append(skel, orig[skelSrc:]...)
	pf.skeleton = skel
	if !sawSOF || len(pf.scans) == 0 {
		return nil, errors.New("プログレッシブ構造ではありません")
	}
	return pf, nil
}

// parseSOFInto は SOF セグメントを frame に読み込む(SOF0/SOF2 共通)。
func parseSOFInto(f *jpegFrame, seg []byte) error {
	if len(seg) < 6 || seg[0] != 8 {
		return errors.New("SOF が短い/8ビット精度でない")
	}
	f.height = int(binary.BigEndian.Uint16(seg[1:]))
	f.width = int(binary.BigEndian.Uint16(seg[3:]))
	nc := int(seg[5])
	if nc < 1 || nc > 4 || len(seg) < 6+nc*3 {
		return errors.New("SOF 成分数が不正")
	}
	f.comps = nil
	f.hmax, f.vmax = 0, 0
	for c := 0; c < nc; c++ {
		o := 6 + c*3
		comp := component{id: int(seg[o]), h: int(seg[o+1] >> 4), v: int(seg[o+1] & 0x0F)}
		if comp.h < 1 || comp.v < 1 || comp.h > 4 || comp.v > 4 {
			return errors.New("サンプリング係数が不正")
		}
		comp.blocksMC = comp.h * comp.v
		if comp.h > f.hmax {
			f.hmax = comp.h
		}
		if comp.v > f.vmax {
			f.vmax = comp.v
		}
		f.comps = append(f.comps, comp)
	}
	return nil
}

func parseDHTInto(f *jpegFrame, seg []byte) error {
	p := 0
	for p < len(seg) {
		if p+17 > len(seg) {
			return errors.New("DHT が短い")
		}
		tc := seg[p] >> 4
		th := seg[p] & 0x0F
		if th > 3 {
			return errors.New("Huffman テーブルID不正")
		}
		bits := seg[p+1 : p+17]
		total := 0
		for _, b := range bits {
			total += int(b)
		}
		if p+17+total > len(seg) {
			return errors.New("DHT vals 範囲外")
		}
		vals := append([]byte(nil), seg[p+17:p+17+total]...)
		ht := buildHuff(bits, vals)
		if tc == 0 {
			f.dc[th] = ht
		} else {
			f.ac[th] = ht
		}
		p += 17 + total
	}
	return nil
}

func parseSOSHeader(f *jpegFrame, seg []byte) (progScan, error) {
	var sc progScan
	if len(seg) < 1 {
		return sc, errors.New("SOS が短い")
	}
	ns := int(seg[0])
	if ns < 1 || ns > 4 || len(seg) < 1+ns*2+3 {
		return sc, errors.New("SOS が短い")
	}
	for s := 0; s < ns; s++ {
		cid := int(seg[1+s*2])
		td := int(seg[2+s*2] >> 4)
		ta := int(seg[2+s*2] & 0x0F)
		if td > 3 || ta > 3 {
			return sc, errors.New("テーブルセレクタ不正")
		}
		ci := -1
		for k := range f.comps {
			if f.comps[k].id == cid {
				ci = k
			}
		}
		if ci < 0 {
			return sc, errors.New("SOS 成分がSOFにない")
		}
		sc.comps = append(sc.comps, scanComp{ci: ci, td: td, ta: ta})
	}
	so := 1 + ns*2
	sc.ss = int(seg[so])
	sc.se = int(seg[so+1])
	sc.ah = int(seg[so+2] >> 4)
	sc.al = int(seg[so+2] & 0x0F)
	if sc.ss > 63 || sc.se > 63 || sc.ss > sc.se {
		return sc, errors.New("Ss/Se 範囲外")
	}
	sc.dc = f.dc
	sc.ac = f.ac
	return sc, nil
}
