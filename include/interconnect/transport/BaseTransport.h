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
#include <boost/concept_check.hpp>
#include <boost/shared_ptr.hpp>

#include "interconnect/accumulo/AccumuloServerFacade.h"
#include "data/extern/boost/SharedPointer.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/TabletClientService.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "ServerTransport.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"

namespace interconnect {

class ThriftTransporter : virtual public ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, std::shared_ptr<cclient::data::Mutation>> {
 protected:
  std::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport;

  AccumuloServerFacade server;

  std::shared_ptr<ServerConnection> clonedConnection;

  virtual void newTransporter(const std::shared_ptr<ServerConnection> &conn) override;

  std::shared_ptr<apache::thrift::transport::TFramedTransport> createTransporter();

  Scan *singleScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    return server.singleScan(request);
  }

  Scan *multiScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    return server.multiScan(request);
  }

 public:

  explicit ThriftTransporter(const std::shared_ptr<ServerConnection> &conn);

  virtual ~ThriftTransporter();

  std::map<std::string, std::string> getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName);

  apache::thrift::transport::TTransport getTransport();

  virtual void authenticate(cclient::data::security::AuthInfo *auth) override;
  void createIfClosed();

  void closeAndCreateClient();

  void createClientService();

  virtual void registerService(const std::string &instance, const std::string &clusterManagers) override {
    createClientService();
    server.registerService(instance, clusterManagers);
  }

  Scan *beginScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    return server.beginScan(request);

  }

  Scan * continueScan(Scan *originalScan) {
    return server.continueScan(originalScan);
  }

  void *write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<std::shared_ptr<cclient::data::Mutation>>> *request) {
    return server.write(auth, request);
  }

  bool dropUser(cclient::data::security::AuthInfo *auth, const std::string &user) {
    return server.dropUser(auth, user);
  }

  bool changeUserPassword(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
    return server.changeUserPassword(auth, user, password);

  }

  bool createUser(cclient::data::security::AuthInfo *auth, const std::string &user, const std::string &password) {
    return server.createUser(auth, user, password);
  }

  std::map<std::string, std::string> getTableConfiguration(cclient::data::security::AuthInfo *auth, const std::string &table) {
    return server.getTableConfiguration(auth, table);
  }

  cclient::data::security::Authorizations *getUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user) {
    return server.getUserAuths(auth, user);
  }

  void changeUserAuths(cclient::data::security::AuthInfo *auth, const std::string &user, cclient::data::security::Authorizations *auths) {
    server.changeUserAuths(auth, user, auths);
  }

  void splitTablet(cclient::data::security::AuthInfo *auth, std::shared_ptr<cclient::data::KeyExtent> extent, const std::string &split) {
    server.splitTablet(auth, extent, split);
  }
  void close() {
    server.close();

    underlyingTransport->close();
  }

  void close(Scan *scan) {
  }

  bool open() {
    return underlyingTransport->isOpen();
  }

  bool isOpen() {
    return underlyingTransport->isOpen();
  }

};
}
#endif
