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
#include "data/extern/boost/SharedPointer.h"
#include "data/extern/thrift/ClientService.h"
#include "data/extern/thrift/TabletClientService.h"
#include "../scanrequest/ScanRequest.h"
#include "../scanrequest/ScanIdentifier.h"
#include "ServerTransport.h"
#include "data/extern/thrift/ThriftWrapper.h"
#include "data/constructs/security/AuthInfo.h"
#include "../Scan.h"
#include "interconnect/accumulo/AccumuloServerTransport.h"

namespace interconnect {

class ThriftTransporter : virtual public ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, cclient::data::Mutation*> {
 protected:
  std::map<cclient::data::security::AuthInfo*, org::apache::accumulo::core::security::thrift::TCredentials> convertedMap;
  boost::shared_ptr<apache::thrift::transport::TTransport> underlyingTransport;
  //std::unique_ptr<org::apache::accumulo::core::client::impl::thrift::ClientServiceClient> client;
  //std::unique_ptr<org::apache::accumulo::core::tabletserver::thrift::TabletClientServiceClient> tserverClient;

  AccumuloServerTransport server;

  std::shared_ptr<ServerConnection> clonedConnection;

  virtual void newTransporter(std::shared_ptr<ServerConnection> conn) {

    clonedConnection = conn;

    boost::shared_ptr<apache::thrift::transport::TSocket> serverTransport(new apache::thrift::transport::TSocket(conn->getHost(), conn->getPort()));

    //serverTransport->setLinger(false,0);
    //serverTransport->setNoDelay(false);
    //serverTransport->setConnTimeout(0);

    boost::shared_ptr<apache::thrift::transport::TTransport> transporty(new apache::thrift::transport::TFramedTransport(serverTransport));

    try {
      std::cout << "attempting to connect to ! to " << conn->getHost() << " and " << conn->getPort() << std::endl;
      transporty->open();

      std::cout << "connected! to " << conn->getHost() << " and " << conn->getPort() << std::endl;
    } catch (const apache::thrift::transport::TTransportException &te) {
      std::cout << conn->getHost() << " host-port " << conn->getPort() << te.what() << " " << std::endl;
      try {
        transporty->close();
      } catch (const apache::thrift::transport::TTransportException &to) {
        std::cout << conn->getHost() << " host-port " << conn->getPort() << te.what() << " " << std::endl;
      }
      throw te;
    }

    underlyingTransport = transporty;

  }

  std::shared_ptr<apache::thrift::transport::TFramedTransport> createTransporter() {
    //boost::shared_ptr<apache::thrift::transport::TTransport> createTransporter() {

//    boost::shared_ptr<apache::thrift::transport::TSocket> serverTransport(new apache::thrift::transport::TSocket(clonedConnection->getHost(), clonedConnection->getPort()));
    auto serverTransport = std::make_shared<apache::thrift::transport::TSocket>(clonedConnection->getHost(), clonedConnection->getPort());

    serverTransport->setLinger(false, 0);
    serverTransport->setNoDelay(true);
    serverTransport->setConnTimeout(0);

    //boost::shared_ptr<apache::thrift::transport::TTransport> transporty(new apache::thrift::transport::TFramedTransport(boost::tools::from_shared_ptr(serverTransport)));
    auto newTransport = std::make_shared<apache::thrift::transport::TFramedTransport>(boost::tools::from_shared_ptr<apache::thrift::transport::TSocket>(serverTransport));

    newTransport->open();

    return newTransport;

  }

  Scan *
  singleScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    return server.singleScan(request);
  }

  Scan *
  multiScan(ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> > *request) {
    return server.multiScan(request);
  }
  org::apache::accumulo::core::security::thrift::TCredentials getOrSetCredentials(cclient::data::security::AuthInfo *convert) {
    std::map<cclient::data::security::AuthInfo*, org::apache::accumulo::core::security::thrift::TCredentials>::iterator it;
    it = convertedMap.find(convert);
    if (it != convertedMap.end()) {
      return it->second;
    }

    org::apache::accumulo::core::security::thrift::TCredentials creds = ThriftWrapper::convert(convert);
    convertedMap.insert(std::pair<cclient::data::security::AuthInfo*, org::apache::accumulo::core::security::thrift::TCredentials>(convert, creds));
    return creds;

  }

 public:

  explicit ThriftTransporter(std::shared_ptr<ServerConnection> conn)
      : interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, cclient::data::Mutation*>(conn) {

    newTransporter(conn);
  }

  virtual ~ThriftTransporter() {
    underlyingTransport->close();
    /*    if (NULL != client)
     delete client;
     */
  }

  std::map<std::string, std::string> getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
    return server.getNamespaceConfiguration(auth, nameSpaceName);
  }

  apache::thrift::transport::TTransport getTransport() {
    return *underlyingTransport;
  }

  virtual void authenticate(cclient::data::security::AuthInfo *auth) override {
    server.authenticate(auth);
  }

  void createIfClosed() {
    if (underlyingTransport.get() != NULL && !underlyingTransport->isOpen()) {

      underlyingTransport = boost::tools::from_shared_ptr<apache::thrift::transport::TTransport>(createTransporter());
      createClientService();
    }
  }

  void closeAndCreateClient() {
    if (underlyingTransport.get() != NULL && underlyingTransport->isOpen()) {
      underlyingTransport->close();
    }

    underlyingTransport = boost::tools::from_shared_ptr<apache::thrift::transport::TTransport>(createTransporter());
    createClientService();
  }

  void createClientService() {

    boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(new apache::thrift::protocol::TCompactProtocol(underlyingTransport));

    server.close();

    server.initialize(protocolPtr);

  }

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

  void *write(cclient::data::security::AuthInfo *auth, std::map<cclient::data::KeyExtent, std::vector<cclient::data::Mutation*>> *request) {
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
