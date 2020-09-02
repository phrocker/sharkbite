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

#ifndef ACCUMULOSERVERTRANSPORT_H
#define ACCUMULOSERVERTRANSPORT_H

#include <algorithm>  // std::random_shuffle
#include <cstdlib>    // std::rand, std::srand
#include <ctime>      // std::time
#include <map>
#include <set>
#include <vector>  // std::vector
//#include <protocol/TBinaryProtocol.h>
#include <concurrency/ThreadManager.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>
#include <sys/time.h>
#include <transport/TBufferTransports.h>
#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransport.h>
#include <transport/TTransportException.h>

#include <chrono>
#include <thread>

#include "../scanrequest/ScanIdentifier.h"
#include "BaseTransport.h"
#include "Transport.h"
#include "data/constructs/IterInfo.h"
#include "data/constructs/configuration/Configuration.h"
#include "data/constructs/inputvalidation.h"
#include "data/constructs/scanstate.h"
#include "data/constructs/security/AuthInfo.h"
#include "data/constructs/tablet/TabletType.h"
#include "data/exceptions/ClientException.h"
#include "data/exceptions/IllegalArgumentException.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/MasterClientService.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/extern/thrift/data_types.h"
#include "data/extern/thrift/master_types.h"
namespace interconnect {

class AccumuloServerFacade : public ThriftTransporter {
 protected:
 public:
  explicit AccumuloServerFacade(const std::shared_ptr<ServerConnection> &conn)
      : ThriftTransporter(conn) {}

  virtual void newTransporter(
      const std::shared_ptr<ServerConnection> &conn) override {}

  virtual void registerService(const std::string &instance,
                               const std::string &clusterManagers) override {}
};

}  // namespace interconnect

#endif  // ACCUMULOSERVERTRANSPORT_H
