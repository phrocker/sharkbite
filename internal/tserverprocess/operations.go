package tserverprocess

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/phrocker/shoal-oss/internal/hostedingest"
	"github.com/phrocker/shoal-oss/internal/ingestservice"
	"github.com/phrocker/shoal-oss/internal/roleops"
	"github.com/phrocker/shoal-oss/internal/tserver"
)

type ScanAdmission interface {
	Accepting() bool
}

type IngestAdmission interface {
	Accepting() bool
	Metrics() ingestservice.Metrics
}

type HostedIngestMetrics interface {
	Metrics() hostedingest.Metrics
}

func OperationsHandler(host *tserver.Host, scans ScanAdmission) http.Handler {
	return OperationsHandlerWithWriteTier(host, scans, nil, nil, false)
}

func OperationsHandlerWithIngest(host *tserver.Host, scans ScanAdmission, ingest IngestAdmission) http.Handler {
	return OperationsHandlerWithWriteTier(host, scans, ingest, nil, ingest != nil)
}

func OperationsHandlerWithWriteTier(
	host *tserver.Host,
	scans ScanAdmission,
	ingest IngestAdmission,
	tablets HostedIngestMetrics,
	requireIngest bool,
) http.Handler {
	dependencies := roleops.NewDependencies(
		"service_lock", "manager_session", "hosted_tablets", "scan_admission",
		"ingest_admission", "wal_authority", "minor_compaction", "backpressure",
	)
	dependencies.SetStarted(true)
	handler := roleops.Handler(dependencies, func(b *strings.Builder) {
		metrics := host.Metrics()
		writeMetric(b, "shoal_tserver_tablets", "state", "loading", metrics.Loading)
		writeMetric(b, "shoal_tserver_tablets", "state", "hosted", metrics.Hosted)
		writeMetric(b, "shoal_tserver_tablets", "state", "unloading", metrics.Unloading)
		writeCounter(b, "shoal_tserver_assignments_total", metrics.Assignments)
		writeCounter(b, "shoal_tserver_loads_total", metrics.Loads)
		writeCounter(b, "shoal_tserver_load_failures_total", metrics.LoadFailures)
		writeCounter(b, "shoal_tserver_unloads_total", metrics.Unloads)
		writeCounter(b, "shoal_tserver_forced_unloads_total", metrics.ForcedUnloads)
		writeCounter(b, "shoal_tserver_rejected_stale_total", metrics.RejectedStale)
		writeCounter(b, "shoal_tserver_rejected_duplicate_total", metrics.RejectedDuplicate)
		writeCounter(b, "shoal_tserver_lock_losses_total", metrics.LockLosses)
		writeCounter(b, "shoal_tserver_dropped_on_lock_loss_total", metrics.DroppedOnLockLoss)
		if ingest != nil {
			im := ingest.Metrics()
			fmt.Fprintf(b, "shoal_tserver_ingest_sessions %d\n", im.ActiveSessions)
			writeCounter(b, "shoal_tserver_ingest_started_total", im.Started)
			writeCounter(b, "shoal_tserver_ingest_batches_total", im.AppliedBatches)
			writeCounter(b, "shoal_tserver_ingest_mutations_total", im.AppliedMutations)
			writeCounter(b, "shoal_tserver_ingest_rejected_batches_total", im.RejectedBatches)
			writeCounter(b, "shoal_tserver_ingest_retried_batches_total", im.RetriedBatches)
			writeCounter(b, "shoal_tserver_ingest_expired_sessions_total", im.ExpiredSessions)
			writeCounter(b, "shoal_tserver_ingest_backpressure_total", im.Backpressure)
		}
		if tablets != nil {
			wm := tablets.Metrics()
			fmt.Fprintf(b, "shoal_tserver_hosted_ingest_tablets %d\n", wm.HostedTablets)
			writeCounter(b, "shoal_tserver_wal_commits_total", wm.WALCommits)
			writeCounter(b, "shoal_tserver_wal_failures_total", wm.WALFailures)
			writeCounter(b, "shoal_tserver_wal_recoveries_total", wm.WALRecoveries)
			writeCounter(b, "shoal_tserver_minc_started_total", wm.MincStarted)
			writeCounter(b, "shoal_tserver_minc_completed_total", wm.MincCompleted)
			writeCounter(b, "shoal_tserver_minc_failures_total", wm.MincFailures)
			writeCounter(b, "shoal_tserver_minc_resumed_total", wm.MincResumed)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, lockHeld := host.Lock()
		_, managerHeld := host.ManagerLock()
		hostMetrics := host.Metrics()
		scanReady := scans != nil && scans.Accepting()
		ingestReady := !requireIngest || (ingest != nil && ingest.Accepting())
		authoritiesReady := !requireIngest || tablets != nil
		dependencies.Set("service_lock", lockHeld, stateDetail(lockHeld, "held", "fencing lost"))
		dependencies.Set("manager_session", managerHeld, stateDetail(managerHeld, "authority observed", "authority unavailable"))
		// Loading and unloading are normal manager-directed lifecycle states,
		// not process-wide outages. Report them without withdrawing unrelated
		// hosted tablets from Kubernetes endpoints.
		dependencies.Set("hosted_tablets", true,
			fmt.Sprintf("hosted=%d loading=%d unloading=%d", hostMetrics.Hosted, hostMetrics.Loading, hostMetrics.Unloading))
		dependencies.Set("scan_admission", scanReady, stateDetail(scanReady, "accepting", "draining"))
		dependencies.Set("ingest_admission", ingestReady, stateDetail(ingestReady, "accepting", "unavailable or draining"))
		dependencies.Set("wal_authority", authoritiesReady, stateDetail(authoritiesReady, "initialized", "unavailable"))
		dependencies.Set("minor_compaction", authoritiesReady, stateDetail(authoritiesReady, "initialized", "unavailable"))
		dependencies.Set("backpressure", ingestReady, stateDetail(ingestReady, "within admission limits", "admission closed"))
		handler.ServeHTTP(w, r)
	})
}

func stateDetail(ready bool, yes, no string) string {
	if ready {
		return yes
	}
	return no
}

func writeMetric(b *strings.Builder, name, label, value string, count int) {
	fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, value, count)
}

func writeCounter(b *strings.Builder, name string, count uint64) {
	fmt.Fprintf(b, "%s %d\n", name, count)
}
