package store

import (
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketFiles  = []byte("files")
	bucketChunks = []byte("chunks")
)

// FileManifest は保存済みファイル1件のメタデータ。
type FileManifest struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	// Chunks は復元順に並んだチャンクハッシュ(hex)の列。
	Chunks []string `json:"chunks"`
}

// ChunkMeta はユニークチャンク1件のメタデータ。
type ChunkMeta struct {
	// Compression は "zstd" または "raw"。
	Compression string `json:"compression"`
	RawSize     int64  `json:"raw_size"`
	StoredSize  int64  `json:"stored_size"`
	RefCount    int64  `json:"ref_count"`
}

func openMetaDB(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("メタデータDBを開けません: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketFiles, bucketChunks} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func getFileManifest(tx *bolt.Tx, id string) (*FileManifest, error) {
	raw := tx.Bucket(bucketFiles).Get([]byte(id))
	if raw == nil {
		return nil, ErrNotFound
	}
	var m FileManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func putFileManifest(tx *bolt.Tx, m *FileManifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketFiles).Put([]byte(m.ID), raw)
}

func getChunkMeta(tx *bolt.Tx, hash string) (*ChunkMeta, error) {
	raw := tx.Bucket(bucketChunks).Get([]byte(hash))
	if raw == nil {
		return nil, nil
	}
	var c ChunkMeta
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func putChunkMeta(tx *bolt.Tx, hash string, c *ChunkMeta) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketChunks).Put([]byte(hash), raw)
}
