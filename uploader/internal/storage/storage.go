package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("object not found")
var ErrConflict = errors.New("remote revision conflict")
var ErrPermission = errors.New("R2 permission denied; check scoped credentials")
var ErrNetwork = errors.New("R2 request failed; check endpoint and connection, then retry")

type Object struct {
	Key      string
	Size     int64
	Hash     string
	ETag     string
	Modified time.Time
}
type PutOptions struct {
	ContentType  string
	CacheControl string
	Hash         string
	Match        string
	Create       bool
}

// CopyOptions describe the public representation to apply to a copied object.
// The source object's checksum is retained as object metadata on the target.
type CopyOptions struct {
	ContentType  string
	CacheControl string
	Hash         string
}

// Store implementations must support concurrent calls from upload workers.
type Store interface {
	Get(context.Context, string) ([]byte, Object, error)
	Head(context.Context, string) (Object, error)
	Put(context.Context, string, io.ReadSeeker, int64, PutOptions) error
	Copy(context.Context, string, string, CopyOptions) error
	Delete(context.Context, string) error
	List(context.Context, string) ([]Object, error)
}
