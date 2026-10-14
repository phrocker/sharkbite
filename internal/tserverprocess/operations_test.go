package tserverprocess

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/tserver"
)

type admission bool

func (a admission) Accepting() bool { return bool(a) }

func TestOperationsReadinessAndMetrics(t *testing.T) {
	host := tserver.NewHost()
	handler := OperationsHandler(host, admission(true))
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready without lock = %d", response.Code)
	}

	lock := tserver.LockID{
		UUID: "11111111-1111-4111-8111-111111111111", Sequence: 1,
	}
	if err := host.AdoptLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(tserver.LockID{
		UUID: "22222222-2222-4222-8222-222222222222", Sequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ready with lock = %d", response.Code)
	}
	manager, _ := host.ManagerLock()
	if _, err := host.Assign(
		tserver.Fence{Server: lock, Manager: manager},
		tserver.Extent{TableID: "1", EndRow: []byte("z")},
	); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("normal tablet loading withdrew process readiness = %d", response.Code)
	}

	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(response.Body.String(), "shoal_tserver_lock_losses_total") {
		t.Fatalf("metrics = %q", response.Body.String())
	}
}

func TestOperationsFailsClosedWhenFencingOrIngestAuthorityIsLost(t *testing.T) {
	host := tserver.NewHost()
	lock := tserver.LockID{
		UUID: "11111111-1111-4111-8111-111111111111", Sequence: 1,
	}
	if err := host.AdoptLock(lock); err != nil {
		t.Fatal(err)
	}
	if err := host.ObserveManagerLock(tserver.LockID{
		UUID: "22222222-2222-4222-8222-222222222222", Sequence: 1,
	}); err != nil {
		t.Fatal(err)
	}
	handler := OperationsHandlerWithWriteTier(host, admission(true), nil, nil, true)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"name":"wal_authority"`) {
		t.Fatalf("missing authority response = %d %q", response.Code, response.Body.String())
	}
	host.LoseLock(lock)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"detail":"fencing lost"`) {
		t.Fatalf("lost fence response = %d %q", response.Code, response.Body.String())
	}
}
