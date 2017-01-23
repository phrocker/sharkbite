/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#ifndef INCLUDE_DATA_SERVICES_ACCUMULO_TABLETSERVER_H_
#define INCLUDE_DATA_SERVICES_ACCUMULO_TABLETSERVER_H_

#include "../Server.h"
#include "../../extern/thrift/TabletClientService.h"

#include <map>
#include <set>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand
//#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>

#include <concurrency/ThreadManager.h>

#include <chrono>
#include <thread>
#include <pthread.h>
#include <sys/time.h>

using namespace std;
using namespace ::apache::thrift;
using namespace ::apache::thrift::protocol;
using namespace ::apache::thrift::transport;
using namespace ::apache::thrift::server;


namespace services
{
namespace accumulo
{
class TabletServer : public Server, public org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceIf
{
protected:

    volatile uint64_t scanId;


public:
    void startScan ( ::org::apache::accumulo::core::data::thrift::InitialScan& /* _return */,
                     const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */,
                     const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */,
                     const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* extent */,
                     const  ::org::apache::accumulo::core::data::thrift::TRange& /* range */,
                     const std::vector< ::org::apache::accumulo::core::data::thrift::TColumn> & /* columns */,
                     const int32_t /* batchSize */, const std::vector< ::org::apache::accumulo::core::data::thrift::IterInfo> & /* ssiList */,
                     const std::map<std::string, std::map<std::string, std::string> > & /* ssio */,
                     const std::vector<std::string> & /* authorizations */,
                     const bool /* waitForWrites */, const bool /* isolated */, const int64_t /* readaheadThreshold */ );

    void continueScan ( ::org::apache::accumulo::core::data::thrift::ScanResult& /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::ScanID /* scanID */ ) {
        return;
    }
    void closeScan ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::ScanID /* scanID */ ) {
        return;
    }
    void startMultiScan ( ::org::apache::accumulo::core::data::thrift::InitialMultiScan& /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const  ::org::apache::accumulo::core::data::thrift::ScanBatch& /* batch */, const std::vector< ::org::apache::accumulo::core::data::thrift::TColumn> & /* columns */, const std::vector< ::org::apache::accumulo::core::data::thrift::IterInfo> & /* ssiList */, const std::map<std::string, std::map<std::string, std::string> > & /* ssio */, const std::vector<std::string> & /* authorizations */, const bool /* waitForWrites */ ) {
        return;
    }
    void continueMultiScan ( ::org::apache::accumulo::core::data::thrift::MultiScanResult& /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::ScanID /* scanID */ ) {
        return;
    }
    void closeMultiScan ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::ScanID /* scanID */ ) {
        return;
    }
    ::org::apache::accumulo::core::data::thrift::UpdateID startUpdate ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */ ) {
        ::org::apache::accumulo::core::data::thrift::UpdateID _return = 0;
        return _return;
    }
    void applyUpdates ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::UpdateID /* updateID */, const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* keyExtent */, const std::vector< ::org::apache::accumulo::core::data::thrift::TMutation> & /* mutations */ ) {
        return;
    }
    void closeUpdate ( ::org::apache::accumulo::core::data::thrift::UpdateErrors& /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::UpdateID /* updateID */ ) {
        return;
    }
    void update ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* keyExtent */, const  ::org::apache::accumulo::core::data::thrift::TMutation& /* mutation */ ) {
        return;
    }
    void startConditionalUpdate ( ::org::apache::accumulo::core::data::thrift::TConditionalSession& /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::vector<std::string> & /* authorizations */, const std::string& /* tableID */ ) {
        return;
    }
    void conditionalUpdate ( std::vector< ::org::apache::accumulo::core::data::thrift::TCMResult> & /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::UpdateID /* sessID */, const  ::org::apache::accumulo::core::data::thrift::CMBatch& /* mutations */, const std::vector<std::string> & /* symbols */ ) {
        return;
    }
    void invalidateConditionalUpdate ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::UpdateID /* sessID */ ) {
        return;
    }
    void closeConditionalUpdate ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::data::thrift::UpdateID /* sessID */ ) {
        return;
    }
    void bulkImport ( std::vector< ::org::apache::accumulo::core::data::thrift::TKeyExtent> & /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const int64_t /* tid */, const  ::org::apache::accumulo::core::data::thrift::TabletFiles& /* files */, const bool /* setTime */ ) {
        return;
    }
    void splitTablet ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* extent */, const std::string& /* splitPoint */ ) {
        return;
    }
    void loadTablet ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */, const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* extent */ ) {
        return;
    }
    void unloadTablet ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */, const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* extent */, const bool /* save */ ) {
        return;
    }
    void flush ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */, const std::string& /* tableId */, const std::string& /* startRow */, const std::string& /* endRow */ ) {
        return;
    }
    void flushTablet ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */, const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* extent */ ) {
        return;
    }
    void chop ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */, const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& /* extent */ ) {
        return;
    }
    void compact ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */, const std::string& /* tableId */, const std::string& /* startRow */, const std::string& /* endRow */ ) {
        return;
    }
    void getTabletServerStatus ( ::org::apache::accumulo::core::master::thrift::TabletServerStatus& /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */ ) {
        return;
    }
    void getTabletStats ( std::vector<TabletStats> & /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* tableId */ ) {
        return;
    }
    void getHistoricalStats ( TabletStats& /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */ ) {
        return;
    }
    void halt ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */ ) {
        return;
    }
    void fastHalt ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::string& /* lock */ ) {
        return;
    }
    void getActiveScans ( std::vector<ActiveScan> & /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */ ) {
        return;
    }
    void getActiveCompactions ( std::vector<ActiveCompaction> & /* _return */, const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */ ) {
        return;
    }
    void removeLogs ( const  ::org::apache::accumulo::trace::thrift::TInfo& /* tinfo */, const  ::org::apache::accumulo::core::security::thrift::TCredentials& /* credentials */, const std::vector<std::string> & /* filenames */ ) {
        return;
    }
    void convert ( org::apache::accumulo::core::data::thrift::TRange range );
    void convert ( org::apache::accumulo::core::data::thrift::TRange range );
};
}
}

#endif /* INCLUDE_DATA_SERVICES_ACCUMULO_TABLETSERVER_H_ */
