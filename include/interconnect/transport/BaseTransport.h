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
#ifndef BASE_TRANSPORT_H
#define BASE_TRANSPORT_H

#include <chrono>
#include <thread>
#include <map>
#include <set>
#include <algorithm>    // std::random_shuffle
#include <vector>       // std::vector
#include <ctime>        // std::time
#include <cstdlib>      // std::rand, std::srand
#include <protocol/TBinaryProtocol.h>
#include <protocol/TCompactProtocol.h>
#include <server/TSimpleServer.h>

#include <transport/TServerSocket.h>
#include <transport/TServerTransport.h>
#include <transport/TTransport.h>
#include <transport/TSocket.h>
#include <transport/TTransportException.h>
#include <transport/TBufferTransports.h>

#include <concurrency/ThreadManager.h>

#include <sys/time.h>

#include "Transport.h"

#include "interconnect/accumulo/AccumuloServerFacade.h"
#include "interconnect/accumulo/AccumuloServerOne.h"
#include "interconnect/accumulo/AccumuloServerTwo.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/TabletClientService.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "ServerTransport.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#include "data/constructs/InstanceVersion.h"

namespace interconnect {

class ThriftTransporter : virtual public ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent,std::shared_ptr<cclient::data::Range>, std::shared_ptr<cclient::data::Mutation>> {
 private:
  std::shared_ptr<logging::Logger> logger;
 protected:
  std::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport;

  std::unique_ptr<AccumuloServerFacade> server;

  std::shared_ptr<ServerConnection> clonedConnection;

  virtual void newTransporter(const std::shared_ptr<ServerConnection> &conn) override;

  std::shared_ptr<apache::thrift::transport::TFramedTransport> createTransporter();

  Scan* singleScan(std::atomic<bool> *isRunning, ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> > *request) {
    return server->singleScan(isRunning, request);
  }

  Scan* multiScan(std::atomic<bool> *isRunning, ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> > *request) {
    return server->multiScan(isRunning, request);
  }

  std::shared_ptr<ServerConnection> getConnection() const {
    return clonedConnection;
  }

  /**
   * Self healing function to switch the interconnect if a failure occurs.
   */
  virtual void switchInterconnect(int version = -1) {
  }

  void switchVersion(int suspectedVersion) {
    switch (suspectedVersion) {
      case -1:
      case 1:
        server = std::make_unique<AccumuloServerFacadeV1>();
        suspectedVersion = 1;
        break;
      case 2:
        server = std::make_unique<AccumuloServerFacadeV2>();
        break;
    }

    if (underlyingTransport.get() != NULL && underlyingTransport->isOpen()) {
      underlyingTransport->close();
    }

    underlyingTransport = createTransporter();

    std::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(new apache::thrift::protocol::TCompactProtocol(underlyingTransport));

    server->initialize(protocolPtr, true);
  }

  void printUncaughtException(std::exception_ptr eptr, const std::string &appendStr) {
    try {
      if (eptr) {
        std::rethrow_exception(eptr);
      }
    } catch (const std::exception &e) {
      logging::LOG_TRACE(logger) << "Caught exception " << e.what() << " " << appendStr;
    }
  }

  template<typename T>
  T callTabletServerApiWithReturn(std::function<T()> fx) {
    try {
      return fx();
    } catch (...) {
      auto eptr = std::current_exception();
      auto suspectedVersion = cclient::data::InstanceVersion::getVersion(getConnection()->toString()) + 1;
      if (suspectedVersion <= 2) {
        switchVersion(suspectedVersion);
        T ret = fx();
        cclient::data::InstanceVersion::setVersion(getConnection()->toString(), suspectedVersion);
        return ret;
      } else {
        std::rethrow_exception(eptr);
      }
    }
  }

  void callTabletServerApi(std::function<void()> fx) {
    try {
      fx();
    } catch (...) {
      auto eptr = std::current_exception();
      auto suspectedVersion = cclient::data::InstanceVersion::getVersion(getConnection()->toString()) + 1;
      if (suspectedVersion <= 2) {
        switchVersion(suspectedVersion);
        fx();
        cclient::data::InstanceVersion::setVersion(getConnection()->toString(), suspectedVersion);
      } else {
        std::rethrow_exception(eptr);
      }
    }
  }

 public:

  explicit ThriftTransporter(const std::shared_ptr<ServerConnection> &conn, bool reg = true);

  virtual ~ThriftTransporter();

  std::map<std::string, std::string> getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName);

  apache::thrift::transport::TTransport getTransport() override;

  virtual void authenticate(cclient::data::security::AuthInfo *auth) override;
  void createIfClosed();

  void closeAndCreateClient();

  void createClientService(bool callRegistration = false);

  virtual void registerService(const std::string &instance, const std::string &clusterManagers) override {
    createClientService(true);
    server->registerService(instance, clusterManagers);
  }

  Scan* beginScan(std::atomic<bool> *isRunning, ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> > *request) override {
    return callTabletServerApiWithReturn<Scan*>([&]() -> Scan* {
      return server->beginScan(isRunning, request);
    });

  }

  Scan* continueScan(Scan *originalScan) {
    return callTabletServerApiWithReturn<Scan*>([&]() -> Scan* {
      return server->continueScan(originalScan);
    });
  }

  void* write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request) override {

    return callTabletServerApiWithReturn<void*>([&]() -> void* {
      return server->write(auth, request);
    });

  }

  bool dropUser(cclient::data::security::AuthInfo *auth, const std::string &user) {
    return callTabletServerApiWithReturn<bool>([&]() -> bool {
      return server->dropUser(auth, user);
    });
  }

  bool changeUserPassword(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
    return callTabletServerApiWithReturn<bool>([&]() -> bool {
      return server->changeUserPassword(auth, user, password);
    });
  }

  bool createUser(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
    return callTabletServerApiWithReturn<bool>([&]() -> bool {
      return server->createUser(auth, user, password);
    });
  }

  std::map<std::string, std::string> getTableConfiguration(cclient::data::security::AuthInfo *auth, const std::string &table) {
    return callTabletServerApiWithReturn<std::map<std::string, std::string>>([&]() -> std::map<std::string, std::string> {
      return server->getTableConfiguration(auth, table);
    });
  }

  cclient::data::security::Authorizations* getUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user) {
    return callTabletServerApiWithReturn<cclient::data::security::Authorizations*>([&]() -> cclient::data::security::Authorizations* {
      return server->getUserAuths(auth, user);
    });
  }

  void changeUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user, cclient::data::security::Authorizations *auths) {
    callTabletServerApi([&]() {
      return server->changeUserAuths(auth, user, auths);
    });

  }

  void splitTablet(cclient::data::security::AuthInfo *auth, std::shared_ptr<cclient::data::KeyExtent> extent, const std::string &split) {
    callTabletServerApi([&]() {
      return server->splitTablet(auth, extent, split);
    });

  }
  void close() override {
    server->close();

    underlyingTransport->close();
  }

  void close(Scan *scan) {
  }

  bool open() override {
    return underlyingTransport->isOpen();
  }

  bool isOpen() override {
    return underlyingTransport->isOpen();
  }

};
}
#endif
