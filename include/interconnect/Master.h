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

#ifndef MASTER_H_
#define MASTER_H_

//#define SIGNED_RIGHT_SHIFT_IS 5
//#define ARITHMETIC_RIGHT_SHIFT 5

#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>

#include "../data/constructs/client/Instance.h"
//#include <server/TSimpleServer.h>

#include <concurrency/ThreadManager.h>
#include <server/TNonblockingServer.h>
#include <transport/TBufferTransports.h>
#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransport.h>

#include <memory>
#include <string>
#include <vector>

#include "../data/constructs/inputvalidation.h"
#include "ClientInterface.h"
#include "TabletServer.h"
#include "namespaceOps/NamespaceOperations.h"
#include "securityOps/SecurityOperations.h"
#include "tableOps/ClientTableOps.h"
#include "transport/AccumuloCoordinatorTransporter.h"

namespace interconnect {

static TransportPool<interconnect::AccumuloCoordinatorTransporter>
    MASTER_COORDINATOR;
/**
 * Provides interconnect for master.
 *
 */
class MasterConnect
    : public RootInterface<interconnect::AccumuloCoordinatorTransporter,
                           cclient::data::KeyValue,
                           scanners::ResultBlock<cclient::data::KeyValue>> {
 public:
  explicit MasterConnect(cclient::data::security::AuthInfo &credentials,
                         std::shared_ptr<cclient::data::Instance> instance);

  /**
   * Constructor
   * @param credentials incoming user credentials
   * @param instance incoming instance
   */
  explicit MasterConnect(cclient::data::security::AuthInfo *credentials,
                         std::shared_ptr<cclient::data::Instance> instance)
      : MasterConnect(*credentials, instance) {}

  MasterConnect() : instance(nullptr) {}

  /**
   * Returns an instance of table operations
   * @param table incoming table
   * @returns instance of table ops for this type of interface
   */
  std::unique_ptr<AccumuloTableOperations> tableOps(const std::string &table);

  /**
   * Returns an instance of namespace  operations
   * @param nm namespace
   * @returns instance of namespace ops for this type of interface
   */
  std::unique_ptr<NamespaceOperations> namespaceOps(const std::string &nm = "");

  /**
   * Returns an instance of security operations
   * @returns instance of security ops for this type of interface
   */
  std::unique_ptr<SecurityOperations> securityOps();

  virtual ~MasterConnect();

  MasterConnect &operator=(const MasterConnect &other) {
    instance = other.instance;
    tabletServers = other.tabletServers;
    cachedTransport = other.cachedTransport;
    setTransport(cachedTransport->getTransporter());
    myTransportPool = other.myTransportPool;
    credentials = other.credentials;
    rangeDef = other.rangeDef;
    tServer = other.tServer;
    serverDef = other.serverDef;
    return *this;
  }

 protected:
  /**
   * Locates tablet servers.
   **/
  void findTservers();

  // instance pointer.
  std::shared_ptr<cclient::data::Instance> instance;

  // tablet servers.
  std::vector<std::shared_ptr<ServerConnection>> tabletServers;

  // cached transport for the master.
  std::shared_ptr<CachedTransport<interconnect::AccumuloCoordinatorTransporter>>
      cachedTransport;

  friend class AccumuloTableOperations;

  friend class SecurityOperations;
};
}  // namespace interconnect
#endif /* MASTER_H_ */
