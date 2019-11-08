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
#ifndef SRC_INTERCONNECT_TRANSPORT_ACCUMULOMASTERTRANSPOTER_H_
#define SRC_INTERCONNECT_TRANSPORT_ACCUMULOMASTERTRANSPOTER_H_

#include <concurrency/ThreadManager.h>

#include <chrono>
#include <thread>
#include <sys/time.h>
#include <locale>

#include <map>
#include <set>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand7

#include "data/constructs/inputvalidation.h"
#include "data/constructs/IterInfo.h"
#include "data/constructs/configuration/Configuration.h"
#include "data/extern/thrift/data_types.h"
#include "data/extern/thrift/tabletserver_types.h"
#include "data/constructs/scanstate.h"
#include "data/exceptions/ClientException.h"
#include "data/exceptions/IllegalArgumentException.h"
#include "data/constructs/tablet/TabletType.h"
#include "../scanrequest/ScanIdentifier.h"

#include "Transport.h"
#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>
#include "data/extern/boost/SharedPointer.h"

#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"

#include "BaseTransport.h"
#include "FateInterface.h"
#include "interconnect/accumulo/AccumuloMasterFacade.h"
#include "interconnect/accumulo/AccumuloMasterOne.h"
#include "interconnect/accumulo/AccumuloMasterTwo.h"
#include "data/constructs/InstanceVersion.h"

namespace interconnect {
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>

/**
 * Represents the interconnect to the master client
 * Purpose: Provides the API connection capabilities to the master client
 * Design: Isa ThriftTransporter and FateInterface, therefore it allows for fate operations
 **/
class AccumuloMasterTransporter : public ThriftTransporter, public FateInterface {
 protected:

  std::unique_ptr<AccumuloMasterFacade> master;

  // thrift master client interface.

  /**
   * creates a new transporter
   * @return new transporter
   **/
  virtual std::shared_ptr<apache::thrift::transport::TTransport> recreateTransport() override {
    return createTransporter();
  }

  /**
   * Creates a new master client for this instance.
   *
   **/
  void createMasterClient() {
    master->createMasterClient(underlyingTransport);
  }

  void recreateMasterClient() override{
    underlyingTransport->close();
    underlyingTransport.reset();
    underlyingTransport = boost::tools::from_shared_ptr<apache::thrift::transport::TTransport>(createTransporter());
    createMasterClient();
  }

  virtual std::string doFateOperations(cclient::data::security::AuthInfo *auth, AccumuloFateOperation type, const std::vector<std::string> &tableArgs,
                                       const std::map<std::string, std::string> &options, bool wait = false) override {
    return master->doFateOperations(auth, type, tableArgs, options, wait);
  }

  void switchInterconnect() override {
    switch (cclient::data::InstanceVersion::getVersion(getConnection()->getHost())) {
        case 1:
          master = std::make_unique<AccumuloMasterFacadeV1>(getConnection()->getHost(),[&]() {
                    recreateMasterClient();
                  },
                         [&]() ->std::shared_ptr<apache::thrift::transport::TTransport> {
                           return createTransporter();
                         });

          break;
        case 2:
          master = std::make_unique<AccumuloMasterFacadeV2>(getConnection()->getHost(),[&]() {
                    recreateMasterClient();
                  },
                         [&]() ->std::shared_ptr<apache::thrift::transport::TTransport> {
                           return createTransporter();
                         });

          break;
      }

    createMasterClient();
  }

 public:

  virtual void registerService(std::string instance, std::string clusterManagers) {
    createMasterClient();
    createClientService(true);

  }

  explicit AccumuloMasterTransporter(std::shared_ptr<ServerConnection> conn)
      : interconnect::ThriftTransporter(conn),
        interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, std::shared_ptr<cclient::data::Mutation>>(conn){
    switchInterconnect();
    createMasterClient();
    createClientService(true);
  }

  bool createTable(cclient::data::security::AuthInfo *auth, const std::string &table) {
    return master->createTable(auth, table);
  }

  bool importDirectory(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &dir, std::string failure_dir, bool setTime) {
    return master->importDirectory(auth, table, dir, failure_dir, setTime);
  }

  bool compactFallBack(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return master->compactFallBack(auth, table, startrow, endrow, wait);
  }

  bool compact(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return master->compact(auth, table, startrow, endrow, wait);
  }

  bool flush(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return master->flush(auth, table, startrow, endrow, wait);
  }

  bool removeTable(cclient::data::security::AuthInfo *auth, const std::string &table) {
    return master->removeTable(auth, table);

  }

  void removeTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property) {
    master->removeTableProperty(auth, table, property);
  }

  void setTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property, const std::string &value) {
    master->setTableProperty(auth, table, property, value);
  }

  /**namespace operations**/

  bool createNamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    return master->createNamespace(auth, name);
  }

  bool deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    return master->deletenamespace(auth, name);
  }

  bool renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) {
    return master->renamenamespace(auth, oldName, newName);
  }

  void removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property) {
    master->removeNamespaceProperty(auth, nameSpaceName, property);
  }

  void setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property, const std::string &value) {
    master->setNamespaceProperty(auth, nameSpaceName, property, value);
  }

  virtual ~AccumuloMasterTransporter() {

  }
};

} /* namespace data */

#endif /* SRC_INTERCONNECT_TRANSPORT_ACCUMULOMASTERTRANSPOTER_H_ */

