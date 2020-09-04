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
#ifndef ACCUMULO_CPP_H_
#define ACCUMULO_CPP_H_

//#define SIGNED_RIGHT_SHIFT_IS 5
//#define ARITHMETIC_RIGHT_SHIFT 5

#include <concurrency/ThreadManager.h>
#include <data/constructs/client/Instance.h>
#include <data/constructs/inputvalidation.h>
#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TNonblockingServer.h>
#include <transport/TBufferTransports.h>
#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransport.h>

#include <memory>
#include <string>
#include <vector>

#include "ClientInterface.h"
#include "TabletServer.h"
#include "data/constructs/coordinator/AccumuloInfo.h"
#include "namespaceOps/NamespaceOperations.h"
#include "python/PythonConnector.h"
#include "python/PythonStructures.h"
#include "securityOps/SecurityOperations.h"
#include "tableOps/ClientTableInfo.h"
#include "tableOps/ClientTableOps.h"
#include "transport/AccumuloCoordinatorTransporter.h"

namespace interconnect {

static TransportPool<interconnect::AccumuloCoordinatorTransporter>
    ACCUMULO_COORDINATOR;
/**
 * Provides interconnect for coordinator.
 *
 */
class AccumuloConnector
    : public RootInterface<interconnect::AccumuloCoordinatorTransporter,
                           cclient::data::KeyValue,
                           scanners::ResultBlock<cclient::data::KeyValue>>,
      PythonConnector {
 public:
  explicit AccumuloConnector(cclient::data::security::AuthInfo &credentials,
                             std::shared_ptr<cclient::data::Instance> instance);

  /**
   * Constructor
   * @param credentials incoming user credentials
   * @param instance incoming instance
   */
  explicit AccumuloConnector(cclient::data::security::AuthInfo *credentials,
                             std::shared_ptr<cclient::data::Instance> instance)
      : AccumuloConnector(*credentials, instance) {}

  /**
   * Returns an instance of table operations
   * @param table incoming table
   * @returns instance of table ops for this type of interface
   */
  std::shared_ptr<AccumuloTableOperations> tableOps(const std::string &table);

  /**
   * Returns an instance of table operations
   * @param table incoming table
   * @returns instance of table ops for this type of interface
   */
  std::shared_ptr<AccumuloTableInfo> tableInfo();

  /**
   * Returns an instance of namespace  operations
   * @param nm namespace
   * @returns instance of namespace ops for this type of interface
   */
  std::shared_ptr<NamespaceOperations> namespaceOps(const std::string &nm = "");

  /**
   * Gets statistics about the accumulo cluster
   * @returns AccumuloInfo object that contains statistics about the cluster
   */
  cclient::data::AccumuloInfo getStatistics();

  /**
   * Returns an instance of security operations
   * @returns instance of security ops for this type of interface
   */
  std::shared_ptr<SecurityOperations> securityOps();

  virtual PythonTableOperations table_operations(
      const std::string &table) override {
    return PythonTableOperations(tableOps(table));
  }

  virtual PythonNamespaceOperations namespace_operations(
      const std::string &nm = "") override {
    return PythonNamespaceOperations(namespaceOps(nm));
  }

  virtual PythonSecurityOperations security_operations() override {
    return PythonSecurityOperations(securityOps());
  }

  virtual PythonTableInfo table_info() override {
    return PythonTableInfo(tableInfo());
  }

  virtual ~AccumuloConnector();

  AccumuloConnector &operator=(const AccumuloConnector &other) {
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

  // cached transport for the coordinator.
  std::shared_ptr<CachedTransport<interconnect::AccumuloCoordinatorTransporter>>
      cachedTransport;

  friend class AccumuloTableOperations;

  friend class SecurityOperations;
};
}  // namespace interconnect

#endif