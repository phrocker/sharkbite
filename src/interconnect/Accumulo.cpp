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
#include "interconnect/Accumulo.h"
#include "interconnect/../data/constructs/security/AuthInfo.h"
#include "interconnect/../scanner/constructs/Results.h"
#include <string>

#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>

#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <server/TNonblockingServer.h>
#include <transport/TBufferTransports.h>

#include <concurrency/ThreadManager.h>

#include <stdio.h>      /* printf, scanf, puts, NULL */
#include <stdlib.h>     /* srand, rand */
#include <time.h>       /* time */

#include "interconnect/../data/constructs/Mutation.h"
#include "interconnect/TabletServer.h"
#include "interconnect/../data/exceptions/ClientException.h"
#include "data/constructs/client/zookeeperinstance.h"

namespace interconnect {

/**
 * Constructor
 * @param credentials incoming user credentials
 * @param instance incoming instance
 */
AccumuloConnector::AccumuloConnector(cclient::data::security::AuthInfo &credentials, std::shared_ptr<cclient::data::Instance> inst)
    : RootInterface<interconnect::AccumuloCoordinatorTransporter, cclient::data::KeyValue, scanners::ResultBlock<cclient::data::KeyValue>>(credentials, inst.get()){

  // copy the instance information
  instance = inst;

  this->myTransportPool = &ACCUMULO_COORDINATOR;
  this->credentials = credentials;

  std::vector<std::string> locations = instance->getMasterLocations();

  if (locations.size() == 0) {
    throw cclient::exceptions::ClientException(NO_MASTER_FOUND);
  }

  std::string master = locations.at(0);

  std::vector<std::string> masterSplit = split(master, ':');

  uint16_t port = atoi(masterSplit.at(1).c_str());
  ConnectorService conn("master", masterSplit.at(0), port);

  // create time out from system configuration
  uint64_t timeout = instance->getConfiguration()->getLong("MASTER_TIMEOUT", 60000);

  // even though we're within the accumulo master, tserver object is just
  // a reference to the connecting server
  tServer = std::make_shared<ServerConnection>(conn.getAddressString(interconnect::INTERCONNECT_TYPES::MASTER_CLIENT), port, timeout);

  cachedTransport = myTransportPool->getTransporter(tServer);

  setTransport(cachedTransport->getTransporter());

  findTservers();

  auto tserverConnection = myTransportPool->getTransporter(&tabletServers, true).second;

  // let's authenticate the user early
  tserverConnection->getTransport()->closeAndCreateClient();
  tserverConnection->getTransport()->authenticate(&credentials);

  myTransportPool->freeTransport(tserverConnection);

}


void AccumuloConnector::findTservers() {
  tabletServers = instance->getServers();
}
/**
 * Returns an instance of table operations
 * @param table incoming table
 * @returns instance of table ops for this type of interface
 */
std::unique_ptr<AccumuloTableOperations> AccumuloConnector::tableOps(const std::string &table) {
  if (IsEmpty(&table))
    throw cclient::exceptions::ClientException(TABLE_OR_NAMESPACE_EMPTY);
  auto tserverConnection = myTransportPool->getTransporter(&tabletServers, true).second;
  return std::unique_ptr<AccumuloTableOperations>(
      new AccumuloTableOperations(AccumuloBaseConnector<interconnect::AccumuloCoordinatorTransporter>::getCredentials(), instance, table, this, tserverConnection, myTransportPool));
}

/**
 * Creates Namespace operations
 * @param nm namespace to create. optional argument.
 * @returns Namespace Operations.
 */
std::unique_ptr<NamespaceOperations> AccumuloConnector::namespaceOps(const std::string &nm) {
  return std::unique_ptr<NamespaceOperations>(new NamespaceOperations(AccumuloBaseConnector<interconnect::AccumuloCoordinatorTransporter>::getCredentials(), nm, instance, this, myTransportPool));
}

/**
 * Create Security Operations
 * @returns new Security operations argument.
 */
std::unique_ptr<SecurityOperations> AccumuloConnector::securityOps() {

  auto ptr = myTransportPool->getTransporter(&tabletServers, true).second;
  return std::unique_ptr<SecurityOperations>(new SecurityOperations(AccumuloBaseConnector<interconnect::AccumuloCoordinatorTransporter>::getCredentials(), instance, ptr, myTransportPool));
}

/**
 * Master connect destructor.
 */
AccumuloConnector::~AccumuloConnector() {
}
}
