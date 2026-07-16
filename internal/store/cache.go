package store

import (
	"container/list"
	"sync"
)

// chunkCache は伸長済みチャンクの LRU キャッシュ。
//
// チャンクはコンテンツアドレス(ハッシュ=内容)なので、エントリが古くなる
// ことはなく無効化は不要。削除済みチャンクのエントリが残っても、同じ
// ハッシュは同じ内容なので正しさに影響しない(LRU で自然に追い出される)。
//
// デルタチェーンの読み出し(ベースをたどる)と、デルタ候補評価時の
// ベース読み込みを高速化する。キャッシュされたスライスは共有されるため
// 呼び出し側は変更してはならない。
type chunkCache struct {
	mu       sync.Mutex
	maxBytes int64
	curBytes int64
	order    *list.List               // 先頭が最近使用
	entries  map[string]*list.Element // hash → element(value は cacheEntry)
}

type cacheEntry struct {
	hash string
	data []byte
}

func newChunkCache(maxBytes int64) *chunkCache {
	return &chunkCache{
		maxBytes: maxBytes,
		order:    list.New(),
		entries:  make(map[string]*list.Element),
	}
}

func (c *chunkCache) get(hash string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[hash]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(cacheEntry).data, true
}

func (c *chunkCache) put(hash string, data []byte) {
	size := int64(len(data))
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[hash]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.entries[hash] = c.order.PushFront(cacheEntry{hash: hash, data: data})
	c.curBytes += size
	for c.curBytes > c.maxBytes {
		el := c.order.Back()
		if el == nil {
			break
		}
		ent := el.Value.(cacheEntry)
		c.order.Remove(el)
		delete(c.entries, ent.hash)
		c.curBytes -= int64(len(ent.data))
	}
}

// remove はキャッシュエントリを明示的に無効化する(リージョン解体時など)。
func (c *chunkCache) remove(hash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[hash]; ok {
		ent := el.Value.(cacheEntry)
		c.order.Remove(el)
		delete(c.entries, hash)
		c.curBytes -= int64(len(ent.data))
	}
}
