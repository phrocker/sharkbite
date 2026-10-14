package accumulo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/zk"
)

func TestNewStaticInstance(t *testing.T) {
	instance, err := NewStaticInstance("accumulo", "uuid-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := instance.Info(); got.Name != "accumulo" || got.ID != "uuid-1" {
		t.Fatalf("Info() = %+v", got)
	}
}

func TestNewZooKeeperInstanceResolvesAndClosesOnce(t *testing.T) {
	fake := &fakeLocator{id: "uuid-1"}
	instance, err := newZooKeeperInstance(
		context.Background(),
		ZooKeeperConfig{
			Servers:      []string{"zk:2181"},
			InstanceName: "accumulo",
		},
		func(cfg ZooKeeperConfig) (locator, error) {
			if cfg.SessionTimeout != DefaultZooKeeperSessionTimeout {
				t.Fatalf("SessionTimeout = %v", cfg.SessionTimeout)
			}
			return fake, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := instance.Info(); got.ID != "uuid-1" {
		t.Fatalf("Info() = %+v", got)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if err := instance.Close(); err != nil {
		t.Fatal(err)
	}
	if fake.closes != 1 {
		t.Fatalf("locator closed %d times, want 1", fake.closes)
	}
}

func TestNewZooKeeperInstancePinsExplicitAndDefaultSessionTimeouts(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{name: "explicit", in: time.Second, want: time.Second},
		{name: "zero uses documented default", want: DefaultZooKeeperSessionTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeLocator{id: "uuid-1"}
			instance, err := newZooKeeperInstance(
				context.Background(),
				ZooKeeperConfig{
					Servers:        []string{"zk:2181"},
					InstanceName:   "accumulo",
					SessionTimeout: test.in,
				},
				func(cfg ZooKeeperConfig) (locator, error) {
					if cfg.SessionTimeout != test.want {
						t.Fatalf("SessionTimeout = %v, want %v", cfg.SessionTimeout, test.want)
					}
					return fake, nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = instance.Close() })
		})
	}
}

func TestNormalizeZooKeeperConfigClonesInputs(t *testing.T) {
	configuration := NewConfiguration()
	configuration.Set("key", "before")
	servers := []string{"zk:2181"}
	cfg, err := normalizeZooKeeperConfig(ZooKeeperConfig{
		Servers:       servers,
		InstanceName:  "accumulo",
		Configuration: configuration,
	})
	if err != nil {
		t.Fatal(err)
	}
	servers[0] = "changed"
	configuration.Set("key", "after")
	if cfg.Servers[0] != "zk:2181" {
		t.Fatalf("Servers = %v", cfg.Servers)
	}
	if got := cfg.Configuration.Get("key"); got != "before" {
		t.Fatalf("Configuration key = %q, want before", got)
	}
}

func TestNewZooKeeperInstanceValidatesConfig(t *testing.T) {
	tests := []ZooKeeperConfig{
		{},
		{Servers: []string{""}, InstanceName: "accumulo"},
		{Servers: []string{"zk:2181"}},
		{Servers: []string{"zk:2181"}, InstanceName: "accumulo", SessionTimeout: -time.Second},
	}
	for _, cfg := range tests {
		_, err := newZooKeeperInstance(context.Background(), cfg, func(ZooKeeperConfig) (locator, error) {
			t.Fatal("factory called for invalid config")
			return nil, nil
		})
		if err == nil {
			t.Fatalf("config %+v succeeded", cfg)
		}
	}
}

func TestNewZooKeeperInstanceHonorsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newZooKeeperInstance(ctx, ZooKeeperConfig{}, func(ZooKeeperConfig) (locator, error) {
		return nil, errors.New("unexpected")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestNewZooKeeperInstanceCancelsDuringResolution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{})
	result := make(chan error, 1)

	go func() {
		_, err := newZooKeeperInstance(
			ctx,
			ZooKeeperConfig{Servers: []string{"zk:2181"}, InstanceName: "accumulo"},
			func(ZooKeeperConfig) (locator, error) {
				close(started)
				<-release
				return &channelLocator{id: "uuid-1", closed: closed}, nil
			},
		)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("locator was not closed after cancelled resolution completed")
	}
}

type fakeLocator struct {
	id     string
	closes int
}

func (l *fakeLocator) InstanceID() string { return l.id }
func (l *fakeLocator) Close()             { l.closes++ }
func (l *fakeLocator) RootTabletLocation(context.Context) (*zk.Location, error) {
	return nil, nil
}
func (l *fakeLocator) InstancePath() string { return "/accumulo/" + l.id }
func (l *fakeLocator) GetRaw(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (l *fakeLocator) Children(context.Context, string) ([]string, error) { return nil, nil }

type channelLocator struct {
	id     string
	closed chan struct{}
}

func (l *channelLocator) InstanceID() string { return l.id }
func (l *channelLocator) Close()             { close(l.closed) }
func (l *channelLocator) RootTabletLocation(context.Context) (*zk.Location, error) {
	return nil, nil
}
func (l *channelLocator) InstancePath() string { return "/accumulo/" + l.id }
func (l *channelLocator) GetRaw(context.Context, string) ([]byte, error) {
	return nil, nil
}
func (l *channelLocator) Children(context.Context, string) ([]string, error) { return nil, nil }
