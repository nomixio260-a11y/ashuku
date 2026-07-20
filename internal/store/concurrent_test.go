package store

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"testing"
)

// TestConcurrentPutDeleteGetSharedChunks は、多数のファイルが同一チャンク群を
// 共有する状況で Put/Delete/Get を並行に走らせ、削除されなかったファイルが
// 常に正しい内容で読めること(=参照カウントの競合による共有チャンクの
// 早期 GC/データ損失が起きないこと)を確認する。store の並行テストが従来
// 皆無だったため(逐次テストしかなく本種のバグが見逃されていた)、-race と
// 併せてこのギャップを埋める。
func TestConcurrentPutDeleteGetSharedChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("並行ストレステストはショートモードでは省略")
	}
	s, err := Open(t.TempDir(), Config{AvgChunkSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	// 全ファイルが共有する 64KB ブロック(小チャンクで ~16 個の共有チャンクに割れる)。
	shared := randomDataSeed(t, 64<<10, 999)

	const G = 6  // goroutine 数
	const N = 15 // goroutine あたりの操作数
	type kept struct {
		id      string
		content []byte
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		survivors = map[string][]byte{}
		errCh     = make(chan error, G*N)
	)
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			var mine []kept
			for i := 0; i < N; i++ {
				// 共有ブロック + ファイル固有の末尾(固有チャンクを 1 つ作る)。
				tail := randomDataSeed(t, 8<<10, int64(g*1000+i))
				content := append(append([]byte(nil), shared...), tail...)
				m, err := s.Put(fmt.Sprintf("f-%d-%d", g, i), bytes.NewReader(content))
				if err != nil {
					errCh <- fmt.Errorf("put g%d i%d: %w", g, i, err)
					return
				}
				mine = append(mine, kept{m.ID, content})
				// 直近以外を時々削除して、共有チャンクの参照カウントを上下させる。
				if i%3 == 0 && len(mine) > 1 {
					v := mine[0]
					if err := s.Delete(v.id); err != nil {
						errCh <- fmt.Errorf("delete %s: %w", v.id, err)
						return
					}
					mine = mine[1:]
				}
				// 生存中のファイルを 1 つ読み、内容一致を即時検証。
				v := mine[len(mine)-1]
				if err := verifyGet(s, v.id, v.content); err != nil {
					errCh <- fmt.Errorf("mid-get: %w", err)
					return
				}
			}
			mu.Lock()
			for _, v := range mine {
				survivors[v.id] = v.content
			}
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	// 最終検証: 生存ファイルはすべて正しい内容で読めなければならない。
	for id, want := range survivors {
		if err := verifyGet(s, id, want); err != nil {
			t.Errorf("生存ファイル %s: %v(共有チャンクの早期 GC=データ損失の疑い)", id, err)
		}
	}
}

func verifyGet(s *Store, id string, want []byte) error {
	_, r, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("get %s: %w", id, err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read %s: %w", id, err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("内容不一致 %s: got %d bytes want %d", id, len(got), len(want))
	}
	return nil
}
