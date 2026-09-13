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

// Store implementations must support concurrent calls from upload workers.
type Store interface {
	Get(context.Context, string) ([]byte, Object, error)
	Head(context.Context, string) (Object, error)
	Put(context.Context, string, io.ReadSeeker, int64, PutOptions) error
	Delete(context.Context, string) error
	List(context.Context, string) ([]Object, error)
}
