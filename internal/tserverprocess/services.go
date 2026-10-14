package tserverprocess

import (
	"errors"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/phrocker/shoal-oss/internal/ingestservice"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletingest"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/tabletscan"
	"github.com/phrocker/shoal-oss/internal/tserver"
	"github.com/phrocker/shoal-oss/internal/tserverrpc"
)

// Services registers and advertises only fully initialized runtime
// authorities. Ingest must be nil until its router, WAL, metadata, memtable,
// and minor-compaction dependencies are all ready.
type Services struct {
	Manager *tserverrpc.Adapter
	Scans   tabletscan.TabletScanClientService
	Ingest  *ingestservice.Service
}

func (s Services) Register(processor *thrift.TMultiplexedProcessor) error {
	if processor == nil || s.Manager == nil || s.Scans == nil {
		return errors.New("tserverprocess: incomplete RPC service set")
	}
	if err := s.Manager.RegisterProcessors(processor); err != nil {
		return err
	}
	processor.RegisterProcessor("scan", tabletscan.NewTabletScanClientServiceProcessor(s.Scans))
	if s.Ingest != nil {
		if !s.Ingest.Accepting() {
			return errors.New("tserverprocess: ingest service is not accepting")
		}
		processor.RegisterProcessor("ingest", tabletingest.NewTabletIngestClientServiceProcessor(s.Ingest))
	}
	return nil
}

func (s Services) LockServices() []tserver.ThriftService {
	services := []tserver.ThriftService{
		tserver.ServiceTabletManagement, tserver.ServiceTabletScan, tserver.ServiceTabletServer,
	}
	if s.Ingest != nil && s.Ingest.Accepting() {
		services = append(services, tserver.ServiceTabletIngest)
	}
	return services
}
