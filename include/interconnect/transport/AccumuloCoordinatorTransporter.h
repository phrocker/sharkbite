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
#ifndef SRC_INTERCONNECT_TRANSPORT_COORDTRANSPOTER_H_
#define SRC_INTERCONNECT_TRANSPORT_COORDTRANSPOTER_H_

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

#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"

#include "BaseTransport.h"
#include "FateInterface.h"
#include "interconnect/accumulo/AccumuloCoordinatorFacade.h"
#include "interconnect/accumulo/AccumuloCoordinatorOne.h"
#include "interconnect/accumulo/AccumuloCoordinatorTwo.h"
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
#include "../exceptions/APIException.h"

/**
 * Represents the interconnect to the coordinator client
 * Purpose: Provides the API connection capabilities to the coordinator client
 * Design: Isa ThriftTransporter and FateInterface, therefore it allows for fate operations
 **/
class AccumuloCoordinatorTransporter : public ThriftTransporter, public FateInterface {
 private:
  std::shared_ptr<logging::Logger> logger;
 protected:
  std::unique_ptr<AccumuloCoordinatorFacade> coordinator;

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
  void createCoordinatorClient() {
    coordinator->createCoordinatorClient(underlyingTransport);
  }

  void recreateCoordinatorClient() override {
    underlyingTransport->close();
    underlyingTransport.reset();
    underlyingTransport = createTransporter();
    createCoordinatorClient();
  }

  virtual std::string doFateOperations(cclient::data::security::AuthInfo *auth, AccumuloFateOperation type, const std::vector<std::string> &tableArgs,
                                       const std::map<std::string, std::string> &options, bool wait = false) override {
    return coordinator->doFateOperations(auth, type, tableArgs, options, wait);
  }

  void switchInterconnect(int version = -1) override {
    if (version == -1) {
      version = cclient::data::InstanceVersion::getVersion(getConnection()->toString());
    }
    switch (version) {
      case -1:
      case 1:
        coordinator = std::make_unique<AccumuloCoordinatorFacadeV1>(getConnection()->getHost(), [&]() {
          recreateCoordinatorClient();
        }
                                                          ,
                                                          [&]() -> std::shared_ptr<apache::thrift::transport::TTransport> {
                                                            return createTransporter();
                                                          });

        break;
      case 2:
        coordinator = std::make_unique<AccumuloCoordinatorFacadeV2>(getConnection()->getHost(), [&]() {
          recreateCoordinatorClient();
        }
                                                          ,
                                                          [&]() -> std::shared_ptr<apache::thrift::transport::TTransport> {
                                                            return createTransporter();
                                                          });

        break;
    }

    createCoordinatorClient();
  }

  bool callApiBool(std::function<bool()> fx) {
    bool ret = false;
    try {
      return fx();
    } catch (const interconnect::APIException &e) {
      auto suspectedVersion = cclient::data::InstanceVersion::getVersion(getConnection()->toString()) + 1;
      switchInterconnect(suspectedVersion);
      ret = fx();
      cclient::data::InstanceVersion::setVersion(getConnection()->toString(), suspectedVersion);
    }
    return ret;
  }

  void callAPi(std::function<void()> fx) {
    try {
      fx();
    } catch (const interconnect::APIException &e) {
      auto suspectedVersion = cclient::data::InstanceVersion::getVersion(getConnection()->toString()) + 1;
      switchInterconnect(suspectedVersion);
      fx();
      cclient::data::InstanceVersion::setVersion(getConnection()->toString(), suspectedVersion);
    }
  }

 public:

  virtual void registerService(std::string instance, std::string clusterManagers) {
    createCoordinatorClient();
    createClientService(false);

  }

  explicit AccumuloCoordinatorTransporter(std::shared_ptr<ServerConnection> conn)
      :
      interconnect::ThriftTransporter(conn, false),
      interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, std::shared_ptr<cclient::data::Range>, std::shared_ptr<cclient::data::Mutation>>(conn),
      logger(logging::LoggerFactory<AccumuloCoordinatorTransporter>::getLogger()) {
    switchInterconnect();
    createCoordinatorClient();
    createClientService(false);
  }

  bool createTable(cclient::data::security::AuthInfo *auth, const std::string &table) {
    return callApiBool([&]() -> bool {
      return coordinator->createTable(auth, table);
    });
  }

  bool importDirectory(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &dir, std::string failure_dir, bool setTime) {
    return callApiBool([&]() -> bool {
      return coordinator->importDirectory(auth, table, dir, failure_dir, setTime);
    });
  }

  bool compactFallBack(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return callApiBool([&]() -> bool {
      return coordinator->compactFallBack(auth, table, startrow, endrow, wait);
    });
  }

  bool compact(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return callApiBool([&]() -> bool {
      return coordinator->compact(auth, table, startrow, endrow, wait);
    });
  }

  bool flush(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &startrow, const std::string &endrow, bool wait) {
    return callApiBool([&]() -> bool {
      return coordinator->flush(auth, table, startrow, endrow, wait);
    });
  }

  bool removeTable(cclient::data::security::AuthInfo *auth, const std::string &table) {
    return callApiBool([&]() -> bool {
      return coordinator->removeTable(auth, table);
    });
  }

  void removeTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property) {
    callAPi([&]() {
      return coordinator->removeTableProperty(auth, table, property);
    });

  }

  void setTableProperty(cclient::data::security::AuthInfo *auth, const std::string &table, const std::string &property, const std::string &value) {
    callAPi([&]() {
      return coordinator->setTableProperty(auth, table, property, value);
    });

  }

  /**namespace operations**/

  bool createNamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    return callApiBool([&]() -> bool {
      return coordinator->createNamespace(auth, name);
    });
  }

  bool deletenamespace(cclient::data::security::AuthInfo *auth, std::string name) {
    return callApiBool([&]() -> bool {
      return coordinator->deletenamespace(auth, name);
    });
  }

  bool renamenamespace(cclient::data::security::AuthInfo *auth, std::string oldName, std::string newName) {
    return callApiBool([&]() -> bool {
      return coordinator->renamenamespace(auth, oldName, newName);
    });
  }

  void removeNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property) {
    callAPi([&]() {
      return coordinator->removeNamespaceProperty(auth, nameSpaceName, property);
    });

  }

  void setNamespaceProperty(cclient::data::security::AuthInfo *auth, std::string nameSpaceName, const std::string &property, const std::string &value) {
    callAPi([&]() {
      return coordinator->setNamespaceProperty(auth, nameSpaceName, property, value);
    });
  }

  virtual ~AccumuloCoordinatorTransporter() {

  }
};

} /* namespace data */

#endif /* SRC_INTERCONNECT_TRANSPORT_COORDTRANSPOTER_H_ */

