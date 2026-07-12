# RESEARCH — 「テラバイトを数ギガバイトに」への研究記録

目標: ディスク容量の少ないサーバーで、テラバイト級の論理データを数ギガバイト級の
物理容量で保持する。本書は (1) 何が理論的に可能/不可能か、(2) ashuku の実装で
実測できた削減率、(3) さらに近づくためのロードマップ、を記録する研究文書である。

## 1. 理論的な前提: 「任意データの1000:1」は不可能、「実データの1000:1」は条件付きで可能

- **鳩の巣原理**: 長さ n ビットの列は 2^n 通りあるが、n−1 ビット以下の出力は
  合計 2^n − 1 通りしかない。すべての入力を縮める可逆写像は存在せず、ある入力を
  縮める圧縮器は必ず別の入力を伸ばす。
- **Kolmogorov複雑性**: ランダムな n ビット列が k ビット以上縮む確率は高々 2^−k。
  一様ランダムデータは高確率で非圧縮。
- **Shannonの情報源符号化定理**: 可逆圧縮の期待符号長は情報源エントロピー H を
  下回れない。**1000:1 は「データの 99.9% が冗長」な場合にのみ達成可能**であり、
  それはアルゴリズムの性能ではなくデータ側の性質である。

参考: [Kolmogorov Complexity primer](https://www.jeremykun.com/2012/04/21/kolmogorov-complexity-a-primer/),
[Compression and Kolmogorov complexity](https://qchu.wordpress.com/2020/09/27/compression-and-kolmogorov-complexity/)

**結論**: 追うべきは「魔法の圧縮アルゴリズム」ではなく、**実データに現実に存在する
99%超の冗長性(バックアップ世代間の重複・類似)を余さず回収する仕組み**である。
バックアップを毎日取るワークロードでは、論理データは世代数に比例して増えるが
実変化は微小なので、世代数 × 変化率次第で 100:1〜1000:1 は物理法則に反しない。

## 2. 文献調査(2026-07-12 実施)

### 2.1 CDC重複排除の実世界削減率

- 実運用バックアップ(EMC Data Domain 1万台超の実測, FAST'12 Wallace et al.):
  **dedup 平均 10.9x / 中央値 8.7x**、dedup後のローカル圧縮 **平均 1.9x**。
  合計でおおよそ **15〜20x** が実運用の代表値。
- データ種別差が大きい: ホームディレクトリ長期保存 14.0x、混合ワークステーション
  11.0x、Eメール 9.6x、DB日次フル 2.2〜5.1x。
- FastCDC (ATC'16) は Rabin CDC と同等の dedup 率でチャンキングを3〜10倍高速化。

出典: [FAST'12 Wallace et al.](https://www.usenix.org/system/files/conference/fast12/wallace2-9-12.pdf),
[FastCDC ATC'16](https://www.usenix.org/system/files/conference/atc16/atc16-paper-xia.pdf)

### 2.2 類似検出+デルタ圧縮(post-dedup delta compression)

- 手法系譜: DERD(super-feature 原型)→ Finesse(FAST'19)→ Odess(検出7.9倍高速)
  → Palantir(ASPLOS'24, 階層的 super-feature)。
- Palantir 実測(バックアップ系7データセット): デルタ圧縮ステージ単体の削減は
  **2.0〜2.8x**、デルタ対象チャンク割合は 29〜57%。エンドツーエンドで
  Finesse 比 +26.5% の圧縮率向上。スループット低下は約8%と小さい。
- 運用上の注意: ベース参照の連鎖により読み出し・GC が複雑化する。

出典: [Finesse FAST'19](https://www.usenix.org/conference/fast19/presentation/zhang),
[Palantir ASPLOS'24](https://dl.acm.org/doi/10.1145/3620665.3640353)
([PDF](https://henryhxu.github.io/share/hongming-asplos24.pdf))

### 2.3 既圧縮フォーマットの可逆再圧縮(precompression)

- **Precomp**: zlib/Deflate(PDF, PNG, ZIP, gzip)等の内部ストリームを展開し、
  ビット一致で再構成できる場合のみ強圧縮をかけ直す。Silesia 67.6MB で
  LZMA2 単体比 **+43%** の削減例。
- **preflate-rs**(Microsoft, Rust): Deflate を「非圧縮データ+数バイトの再構成情報」
  に分解。実装部品として最有望。
- **Lepton**(Dropbox): JPEG を**平均22%**可逆削減。160億ファイル・数PBの実運用実績。

出典: [precomp-cpp](https://github.com/schnaader/precomp-cpp),
[preflate-rs](https://github.com/microsoft/preflate-rs),
[Lepton](https://dropbox.tech/infrastructure/lepton-image-compression-saving-22-losslessly-from-images-at-15mbs)

### 2.4 極限圧縮アルゴリズム(enwik9 = 1GB 英語Wikipedia, LTCB)

| 圧縮器 | 圧縮率 | コスト |
|---|---|---|
| nncp(NN) | ~9.3x | GPU前提・極低速 |
| cmix | ~9.2x | RAM 31GB・1GBに約1週間 |
| zpaq(最大) | ~7.0x | RAM 14GB・zstdの数百倍のCPU |
| zstd 高レベル | ~4.6x | 実用速度 |

**zstd 高レベルと cmix の差は約2倍、速度差は4〜5桁**。サービス用途では
zstd 高レベル+長距離マッチング+辞書がパレート最適。zpaq級はコールド
アーカイブ層専用と割り切るべき。

出典: [Large Text Compression Benchmark](https://mattmahoney.net/dc/text.html)

### 2.5 商用ストレージの公称値(期待値の相場)

- Pure Storage: 平均 **5:1**(dedup+圧縮)、総合効率 10:1
- Dell EMC Data Domain: 公称「最大50:1」、実際は 2:1〜50:1(世代保持条件に依存)
- Red Hat VDO: VM/コンテナ 10:1、汎用 3:1 が推奨プロビジョニング比
- 業界相場: **プライマリ 2〜5x、バックアップ(世代保持)10〜30x** が現実的範囲

出典: [Pure Storage blog](https://blog.purestorage.com/purely-technical/understanding-deduplication-ratios/),
[Red Hat VDO docs](https://docs.redhat.com/en/documentation/red_hat_enterprise_linux/8/html/deduplicating_and_compressing_storage/deploying-vdo_deduplicating-and-compressing-storage)

## 3. ashuku の実装と実測

### 3.1 実装した削減パイプライン

1. **FastCDC 重複排除**(平均チャンク 1MiB、`-chunk-avg` で調整可)
2. **類似チャンク検出**: gear ローリングハッシュのサンプル値に線形変換をかけた
   最大値を特徴(super-feature ×3)とし、bbolt 索引で候補検索(Odess/Finesse 系)
3. **デルタ圧縮**: 類似ベースを zstd 辞書として差分のみ保存。
   デルタチェーン(深さ上限 `-delta-depth`, デフォルト16)で直近世代との差分を維持
4. **zstd 圧縮**(fast/balanced/max)+ **raw フォールバック**(圧縮不能データの無膨張保証)
5. **参照カウントGC**(デルタのベース参照はカスケード解放)

### 3.2 ベンチマーク結果(`go run ./cmd/ashuku-bench`, 合成データセット)

測定条件: 各データセット32MiB(バックアップは16MiB×20世代=320MiB)、デルタチェーン深さ16。

| データセット | 設定 | 論理サイズ | 物理サイズ | 削減倍率 | スループット |
|---|---|---:|---:|---:|---:|
| ログ(反復構造) | fast | 32.0 MiB | 6.3 MiB | **5.1x** | 61 MB/s |
| ログ(反復構造) | balanced | 32.0 MiB | 6.1 MiB | **5.3x** | 35 MB/s |
| ログ(反復構造) | max | 32.0 MiB | 5.9 MiB | **5.5x** | 13 MB/s |
| 疑似テキスト | balanced | 32.0 MiB | 10.7 MiB | **3.0x** | 36 MB/s |
| 疑似テキスト | max | 32.0 MiB | 10.2 MiB | **3.1x** | 12 MB/s |
| 乱数(圧縮不能) | balanced | 32.0 MiB | 32.0 MiB | **1.0x** | 99 MB/s |
| バックアップ20世代(全域変更) | balanced | 320.0 MiB | 14.4 MiB | **22.3x** | 15 MB/s |
| バックアップ20世代(全域変更) | max | 320.0 MiB | 14.0 MiB | **22.8x** | 7 MB/s |
| バックアップ20世代(全域変更) | balanced+delta無効 | 320.0 MiB | 177.4 MiB | **1.8x** | 45 MB/s |
| バックアップ20世代(局所変更) | balanced | 320.0 MiB | 8.9 MiB | **36.0x** | 35 MB/s |
| バックアップ20世代(局所変更) | max | 320.0 MiB | 8.7 MiB | **36.8x** | 13 MB/s |
| バックアップ20世代(局所変更) | balanced+delta無効 | 320.0 MiB | 93.4 MiB | **3.4x** | 72 MB/s |

(「全域変更」= 毎世代0.1%の編集を全体に散らす=完全一致dedupが全滅する最悪ケース。
「局所変更」= 編集を少数ブロックに集中=現実の日次バックアップに近いケース)

ポイント:
- **デルタ圧縮の効果**: バックアップ世代ワークロードで delta 無効比 数倍〜10倍の改善。
  完全一致 dedup が全滅する「全チャンクに編集が散る」最悪ケースでも類似検出が救う。
- **raw フォールバック**: 乱数(圧縮不能)データで物理≒論理を保証(膨張なし)。
- **fast レベルはデルタ効率が悪い**: デルタ圧縮の質が下がり閾値で不採用が増える。
  デルタを活かすなら balanced 以上を推奨(デフォルト)。

### 3.3 世代数スケーリング(「テラ→数ギガ」の核心)

世代数を増やしたときの削減倍率(局所変更・balanced・深さ16):

| 世代数 | 論理サイズ | 物理サイズ | 削減倍率 |
|---:|---:|---:|---:|
| 5 | 80.0 MiB | 8.6 MiB | **9.3x** |
| 20 | 320.0 MiB | 8.9 MiB | **36.0x** |
| 50 | (測定中) | | |
| 100 | (測定中) | | |
| 200 | (測定中) | | |

5→20世代で物理増分はわずか0.3MiB(初回フルが支配的で、以降の世代は
ほぼ差分コストのみ)。**削減倍率は世代数にほぼ比例して伸びる**ことが確認できる。

設計判断の実測記録(100世代・局所変更・balanced):

| 設計 | 削減倍率 | 備考 |
|---|---:|---|
| デルタなし(dedup+zstdのみ) | ~3x | 境界シフトで dedup が部分的に無効化 |
| デルタ(ベース固定=最古世代) | 26.6x | ドリフト蓄積でデルタが世代とともに肥大 |
| キーフレーム方式(深さ1+50%閾値再アンカー) | 26.6x | 再アンカーが遅く、ドリフト解消せず |
| **デルタチェーン 深さ8** | 31.6x | 再アンカー(完全コピー)コストが支配的 |
| **デルタチェーン 深さ32** | **82.7x** | 読み出し最大32チャンク復元とのトレードオフ |

採用: **チェーン方式・デフォルト深さ16**(削減率と読み出しコストのバランス点)。
`-delta-depth 32` 以上でアーカイブ特化にできる。

### 3.4 実データでの検証(開発時のE2Eテスト)

- 高冗長ログ 305MB → 物理 6.8MB(**44.7x**)
- 121MB の tar の先頭に 1KB 挿入した「2世代目」→ 物理増分 224KB(**0.18%**)
- 乱数 50MB → 物理増分ちょうど 50MB(膨張ゼロ)
- 全ダウンロードで sha256 完全一致、削除で物理容量が正確に減少

### 3.5 「テラ→数ギガ」への現実的な試算

実測とFAST'12の実運用統計に基づく試算(20GBの業務データを毎日フルバックアップ、
日次変化率 0.5%、365世代 ≈ 論理 7.3TB):

- 世代間はデルタ/dedupでほぼ変化分のみ: 365 × 20GB × 0.5% ÷ 3(圧縮) ≈ 12GB
- 初回フル: 20GB ÷ 3 ≈ 6.7GB
- **物理合計 ≈ 19GB(論理 7.3TB → 削減率 ~380x)**

変化率 0.1% なら物理 ≈ 9GB(**~810x**)。つまり**「テラ級の論理データを数〜数十ギガ
の物理で保持」は、世代保持型ワークロードで現実に達成可能**であり、ashuku の
実装はそのための機構(dedup+類似デルタ+チェーン)を備えた。一方、**一度きりの
ユニークデータ(乱数・既圧縮メディア)のテラバイトはどの技術でも縮まない**。
これは実装の限界ではなく情報理論の限界である。

## 4. ロードマップ(削減率をさらに上げる)

文献調査の優先度リストに基づく:

1. **[P0] チャンクサイズ最適化** — 実装済み(`-chunk-avg`)。小チャンク(64〜256KB)は
   dedup 率を上げるがメタデータ増。ワークロード別の推奨値をベンチで確立する。
2. **[P1] 圧縮リージョン集約** — 小チャンク運用時、ユニークチャンクを ~128KB
   リージョンに集めて一括圧縮(Data Domain 方式)すると小チャンクの圧縮率劣化を
   相殺できる。
3. **[P1] zstd 辞書学習** — データ種別ごとに学習した辞書で小チャンクの圧縮率を改善。
4. **[P2] Deflate 系 precompression(preflate 相当)** — gzip/PNG/PDF 内部ストリームを
   展開して dedup/デルタ/強圧縮の対象にする。gzip 化されたログのバックアップ等で
   劇的に効く(Silesia 実測 +43%)。ビット一致検証を必須とし、失敗時は素通し。
5. **[P2] JPEG 可逆再圧縮(Lepton 系, 平均22%)** — 画像が支配的な場合のみ。
6. **[P3] コールド層の超高圧縮(zpaq級)** — アクセス頻度極小の層を分離できる場合のみ。
   CPU数百倍で +50% 程度なので優先度は低い。
7. **[運用] 期待値の可視化** — `/api/v1/stats` の削減率をワークロード判断に使う。
   削減率はアルゴリズムではなくデータで決まるため、公称値は必ず前提条件付きで示す。

## 5. 再現方法

```sh
go run ./cmd/ashuku-bench                     # 全データセット × 全設定
go run ./cmd/ashuku-bench -gens 100 -only 局所変更   # 世代スケーリング
go run ./cmd/ashuku-bench -input yourdata.tar # 実データ
```
