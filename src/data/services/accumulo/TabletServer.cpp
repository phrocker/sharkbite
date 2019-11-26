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


#include "data/services/accumulo/TabletServer.h"
#include "data/services/accumulo/ScanSession.h"
#include "data/services/Server.h"
#include "data/extern/thrift/TabletClientService.h"

#include <map>
#include <set>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand
#include <chrono>
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


#include "data/extern/thrift/ThriftWrapper.h"

using namespace std;
using namespace ::apache::thrift;
using namespace ::apache::thrift::protocol;
using namespace ::apache::thrift::transport;
using namespace ::apache::thrift::server;


namespace services
{
namespace accumulo
{

using namespace interconnect;

void TabletServer::startScan ( ::org::apache::accumulo::core::data::thrift::InitialScan&  result ,
                               const  ::org::apache::accumulo::trace::thrift::TInfo&  tinfo ,
                               const  ::org::apache::accumulo::core::security::thrift::TCredentials&  credentials ,
                               const  ::org::apache::accumulo::core::data::thrift::TKeyExtent& extent ,
                               const  ::org::apache::accumulo::core::data::thrift::TRange&  range ,
                               const std::vector< ::org::apache::accumulo::core::data::thrift::TColumn> & columns ,
                               const int32_t  batchSize , const std::vector< ::org::apache::accumulo::core::data::thrift::IterInfo> &  ssiList ,
                               const std::map<std::string, std::map<std::string, std::string> > &  ssio ,
                               const std::vector<std::string> & authorizations ,
                               const bool waitForWrites , const bool  isolated , const int64_t readaheadThreshold  ) {

    Scan *scan = new Scan(scanId++);


    scan->addRange( ThriftWrapper::convert(range) );
    scan->setCredentials( ThriftWrapper::convert(credentials) );
    scan->setExtent( ThriftWrapper::convert(extent) );
    scan->setColumns( ThriftWrapper::convert(columns) );
    scan->setIterators( ThriftWrapper::convert(ssiList ) );
    scan->setIteratorOptions(ssio);
    scan->setAuthorizations( authorizations );


    return;
}
}
}
