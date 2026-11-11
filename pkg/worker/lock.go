package worker

import (
	"context"
	"sync"
)

type contextLock struct {
	once sync.Once
	gate chan struct{}
}

func (m *contextLock) lock(ctx context.Context) error {
	m.once.Do(func() { m.gate = make(chan struct{}, 1) })
	select {
	case m.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *contextLock) Unlock() { <-m.gate }
