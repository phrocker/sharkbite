package accumulo

import (
	"fmt"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/transportpool"
)

const (
	// DefaultAccumuloVersion is the Accumulo protocol version used when
	// ConnectorOptions.AccumuloVersion is empty.
	DefaultAccumuloVersion = "4.0.0-SNAPSHOT"

	defaultDialTimeout        = 30 * time.Second
	defaultIdleTimeout        = time.Minute
	defaultMaxIdlePerEndpoint = 2
)

// ZooKeeperConfig identifies an Accumulo instance through ZooKeeper.
type ZooKeeperConfig struct {
	Servers        []string
	InstanceName   string
	SessionTimeout time.Duration
	InstanceSecret string

	// Configuration is optional client configuration carried by the
	// resulting Instance and read back through Instance.Configuration. The
	// instance stores a copy, so later mutations of the caller's
	// Configuration do not change what the instance reports. A nil
	// Configuration yields an empty one.
	Configuration *Configuration
}

// ConnectorOptions controls transport behavior for a Connector.
type ConnectorOptions struct {
	AccumuloVersion    string
	DialTimeout        time.Duration
	IdleTimeout        time.Duration
	MaxIdlePerEndpoint int

	// DisableTransportReuse closes transports when their lease is returned.
	// MaxIdlePerEndpoint must be zero when this is true.
	DisableTransportReuse bool

	// DisableIdleExpiration retains pooled transports until eviction or
	// connector shutdown. IdleTimeout must be zero when this is true.
	DisableIdleExpiration bool
}

type normalizedConnectorOptions struct {
	accumuloVersion string
	dialTimeout     time.Duration
	poolConfig      transportpool.Config
}

func normalizeConnectorOptions(opts ConnectorOptions) (normalizedConnectorOptions, error) {
	if opts.AccumuloVersion == "" {
		opts.AccumuloVersion = DefaultAccumuloVersion
	}
	if !strings.HasPrefix(opts.AccumuloVersion, "4.") {
		return normalizedConnectorOptions{}, fmt.Errorf(
			"%w: only Accumulo 4.x is currently supported, got %q",
			ErrUnsupportedVersion,
			opts.AccumuloVersion,
		)
	}
	if opts.DialTimeout < 0 {
		return normalizedConnectorOptions{}, fmt.Errorf("accumulo: DialTimeout must not be negative")
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = defaultDialTimeout
	}
	if opts.IdleTimeout < 0 {
		return normalizedConnectorOptions{}, fmt.Errorf("accumulo: IdleTimeout must not be negative")
	}
	if opts.DisableIdleExpiration && opts.IdleTimeout != 0 {
		return normalizedConnectorOptions{}, fmt.Errorf(
			"accumulo: IdleTimeout must be zero when DisableIdleExpiration is true",
		)
	}
	if opts.DisableIdleExpiration {
		opts.IdleTimeout = 0
	} else if opts.IdleTimeout == 0 {
		opts.IdleTimeout = defaultIdleTimeout
	}
	if opts.MaxIdlePerEndpoint < 0 {
		return normalizedConnectorOptions{}, fmt.Errorf("accumulo: MaxIdlePerEndpoint must not be negative")
	}
	if opts.DisableTransportReuse && opts.MaxIdlePerEndpoint != 0 {
		return normalizedConnectorOptions{}, fmt.Errorf(
			"accumulo: MaxIdlePerEndpoint must be zero when DisableTransportReuse is true",
		)
	}
	if opts.DisableTransportReuse {
		opts.MaxIdlePerEndpoint = 0
	} else if opts.MaxIdlePerEndpoint == 0 {
		opts.MaxIdlePerEndpoint = defaultMaxIdlePerEndpoint
	}

	return normalizedConnectorOptions{
		accumuloVersion: opts.AccumuloVersion,
		dialTimeout:     opts.DialTimeout,
		poolConfig: transportpool.Config{
			IdleTimeout:        opts.IdleTimeout,
			MaxIdlePerEndpoint: opts.MaxIdlePerEndpoint,
		},
	}, nil
}
