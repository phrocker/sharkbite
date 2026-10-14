package zk

import (
	"context"
	"errors"
	"fmt"

	gozk "github.com/go-zookeeper/zk"
)

// SharedSession exposes the Locator's authenticated, reusable ZooKeeper
// session to components that must share one session identity: ServiceLock and
// manager-lock observation. Locator remains the owner and closes it.
type SharedSession struct {
	locator *Locator
}

func (l *Locator) SharedSession() (*SharedSession, error) {
	if l == nil || l.isClosed() || l.conn == nil {
		return nil, ErrClosed
	}
	return &SharedSession{locator: l}, nil
}

func (s *SharedSession) InstancePath() string {
	if s == nil || s.locator == nil {
		return ""
	}
	return s.locator.InstancePath()
}

func (s *SharedSession) Create(path string, data []byte, flags int32, acl []gozk.ACL) (string, error) {
	if err := s.valid(); err != nil {
		return "", err
	}
	return s.locator.conn.Create(path, data, flags, acl)
}

func (s *SharedSession) ChildrenRaw(path string) ([]string, *gozk.Stat, error) {
	if err := s.valid(); err != nil {
		return nil, nil, err
	}
	return s.locator.conn.Children(path)
}

// Children implements both the context-aware manager reader and the raw
// ServiceLock shape through the separate LockChildren adapter below.
func (s *SharedSession) Children(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	children, _, err := s.ChildrenRaw(path)
	if err != nil {
		return nil, fmt.Errorf("children %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return children, nil
}

func (s *SharedSession) GetW(path string) ([]byte, *gozk.Stat, <-chan gozk.Event, error) {
	if err := s.valid(); err != nil {
		return nil, nil, nil, err
	}
	return s.locator.conn.GetW(path)
}

func (s *SharedSession) Get(path string) ([]byte, *gozk.Stat, error) {
	if err := s.valid(); err != nil {
		return nil, nil, err
	}
	return s.locator.conn.Get(path)
}

func (s *SharedSession) Set(path string, data []byte, version int32) (*gozk.Stat, error) {
	if err := s.valid(); err != nil {
		return nil, err
	}
	return s.locator.conn.Set(path, data, version)
}

func (s *SharedSession) SessionID() int64 {
	if s == nil || s.locator == nil || s.locator.conn == nil {
		return 0
	}
	return s.locator.conn.SessionID()
}

func (s *SharedSession) Delete(path string, version int32) error {
	if err := s.valid(); err != nil {
		return err
	}
	return s.locator.conn.Delete(path, version)
}

func (s *SharedSession) valid() error {
	if s == nil || s.locator == nil || s.locator.isClosed() || s.locator.conn == nil {
		return ErrClosed
	}
	return nil
}

// LockSession adapts SharedSession to tserver.LockConn, whose Children method
// intentionally mirrors go-zookeeper rather than the context-aware reader.
type LockSession struct {
	*SharedSession
}

func (s LockSession) Children(path string) ([]string, *gozk.Stat, error) {
	if s.SharedSession == nil {
		return nil, nil, errors.New("zk: nil shared session")
	}
	return s.ChildrenRaw(path)
}
