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

#ifndef TABLETSERVER_H_
#define TABLETSERVER_H_ 1

#ifndef SIGNED_RIGHT_SHIFT_IS
    #define SIGNED_RIGHT_SHIFT_IS 5
#endif

#ifndef ARITHMETIC_RIGHT_SHIFT
    #define ARITHMETIC_RIGHT_SHIFT 5
#endif

#include <chrono>
#include <string>
#include <thread>

#include "../data/constructs/IterInfo.h"
#include "../data/constructs/client/TabletServerMutations.h"
#include "../data/constructs/configuration/Configuration.h"
#include "../data/constructs/inputvalidation.h"
#include "../data/constructs/scanstate.h"
#include "../data/constructs/server/RangeDefinition.h"
#include "../data/constructs/server/ServerDefinition.h"
#include "../data/constructs/tablet/TabletType.h"
#include "../data/exceptions/ClientException.h"
#include "../data/exceptions/IllegalArgumentException.h"
#include "../data/exceptions/NotServingException.h"
#include "../data/extern/thrift/ClientService.h"
#include "../data/extern/thrift/TabletClientService.h"
#include "../data/extern/thrift/data_types.h"
#include "../interconnect/ClientInterface.h"
#include "AccumuloConnector.h"
#include "InterConnect.h"
#include "ScanArbiter.h"
#include "TransportPool.h"
#include "interconnect/scanrequest/ScanRequest.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#include "transport/BaseTransport.h"
#include "transport/ServerTransport.h"

namespace interconnect {

static TransportPool<ThriftTransporter> CLUSTER_COORDINATOR;

extern void closeAllThriftConnections();

class ServerInterconnect
    : public AccumuloBaseConnector<interconnect::ThriftTransporter> {
 private:
  std::shared_ptr<logging::Logger> logger;

 protected:
  explicit ServerInterconnect(cclient::data::security::AuthInfo *creds,
                              TransportPool<ThriftTransporter> *
                                  distributedConnector = &CLUSTER_COORDINATOR) {
    myTransportPool = distributedConnector;
    this->credentials = *creds;
    myTransport = NULL;
  }
  std::shared_ptr<CachedTransport<ThriftTransporter>> myTransport;

  void recreateConnection(bool errorOcurred = false);

 public:
  explicit ServerInterconnect(const std::string host, const int port,
                              const cclient::impl::Configuration *conf,
                              TransportPool<ThriftTransporter>
                                  *distributedConnector = &CLUSTER_COORDINATOR);

  explicit ServerInterconnect(
      std::shared_ptr<cclient::data::tserver::RangeDefinition> rangeDef,
      const cclient::impl::Configuration *conf,
      TransportPool<ThriftTransporter> *distributedConnector =
          &CLUSTER_COORDINATOR);

  Scan *hedgedScan(
      std::shared_ptr<interconnect::ScanArbiter> &arbiter,
      std::atomic<bool> *isRunning,
      const std::vector<cclient::data::Column> &cols,
      const std::vector<cclient::data::IterInfo> &serverSideIterators,
      cclient::data::IterInfo &versioningIterator, uint32_t batchSize = 1000,
      bool disableRpc = false);

  Scan *scan(std::atomic<bool> *isRunning,
             const std::vector<cclient::data::Column> &cols,
             const std::vector<cclient::data::IterInfo> &serverSideIterators,
             uint32_t batchSize = 1000);

  explicit ServerInterconnect(
      std::shared_ptr<cclient::data::tserver::ServerDefinition> rangeDef,
      const cclient::impl::Configuration *conf,
      TransportPool<ThriftTransporter> *distributedConnector =
          &CLUSTER_COORDINATOR);

  Scan *scan(std::atomic<bool> *isRunning);

  Scan *continueScan(Scan *scan);

  std::shared_ptr<cclient::data::TabletServerMutations> write(
      std::shared_ptr<cclient::data::TabletServerMutations> mutations);

  void halt() {}

  void authenticate(cclient::data::security::AuthInfo *credentials);

  virtual void authenticate(const std::string &username,
                            const std::string &password) override {}

  virtual ~ServerInterconnect();
};

}  // namespace interconnect
#endif /* TABLETSERVER_H_ */
