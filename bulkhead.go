package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// BulkheadConfig configures a client-scoped logical-operation concurrency
// limit.
type BulkheadConfig struct {
	// MaxConcurrent is the maximum number of admitted operations. It must be
	// positive.
	MaxConcurrent int
}

type bulkhead struct {
	slots chan struct{}
}

type bulkheadDoer struct {
	next     HTTPDoer
	bulkhead *bulkhead
}

type bulkheadBody struct {
	body    io.ReadCloser
	release func()
}

func newBulkhead(config BulkheadConfig) *bulkhead {
	return &bulkhead{slots: make(chan struct{}, config.MaxConcurrent)}
}

func (bulkhead *bulkhead) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case bulkhead.slots <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-bulkhead.slots
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (doer *bulkheadDoer) Do(request *http.Request) (response *http.Response, err error) {
	if request == nil {
		return doer.next.Do(nil)
	}
	if err := doer.bulkhead.acquire(request.Context()); err != nil {
		return nil, err
	}

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			<-doer.bulkhead.slots
		})
	}
	transferred := false
	defer func() {
		if !transferred {
			release()
		}
	}()

	response, err = doer.next.Do(request)
	if err != nil || response == nil || response.Body == nil || response.Body == http.NoBody {
		return response, err
	}
	response.Body = &bulkheadBody{
		body:    response.Body,
		release: release,
	}
	transferred = true
	return response, nil
}

func (body *bulkheadBody) Read(buffer []byte) (n int, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			body.release()
			panic(recovered)
		}
	}()
	n, err = body.body.Read(buffer)
	if err != nil {
		body.release()
	}
	return n, err
}

func (body *bulkheadBody) Close() (err error) {
	defer body.release()
	return body.body.Close()
}

func (config BulkheadConfig) validate() error {
	if config.MaxConcurrent < 1 {
		return errors.New("max concurrent operations must be positive")
	}
	return nil
}
