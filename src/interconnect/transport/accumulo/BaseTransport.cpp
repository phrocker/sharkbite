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
#include "interconnect/transport/BaseTransport.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace interconnect {

ThriftTransporter::ThriftTransporter(const std::shared_ptr<ServerConnection> &conn)
    : interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, std::shared_ptr<cclient::data::Mutation>>(conn),
      logger(logging::LoggerFactory<ThriftTransporter>::getLogger()) {
  switch (cclient::data::InstanceVersion::getVersion(conn->getHost())) {
    case 1:
      server = std::make_unique<AccumuloServerFacadeV1>();
      break;
    case 2:
      server = std::make_unique<AccumuloServerFacadeV2>();
      break;
  }
  newTransporter(conn);
}

ThriftTransporter::~ThriftTransporter() {
  underlyingTransport->close();
}

std::map<std::string, std::string> ThriftTransporter::getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
  return server->getNamespaceConfiguration(auth, nameSpaceName);
}

apache::thrift::transport::TTransport ThriftTransporter::getTransport() {
  return *underlyingTransport;
}

void ThriftTransporter::authenticate(cclient::data::security::AuthInfo *auth) {
  server->authenticate(auth);
}

void ThriftTransporter::createIfClosed() {
  if (underlyingTransport.get() != NULL && !underlyingTransport->isOpen()) {

    underlyingTransport = createTransporter();
    createClientService();
  }
}

void ThriftTransporter::closeAndCreateClient() {
  if (underlyingTransport.get() != NULL && underlyingTransport->isOpen()) {
    underlyingTransport->close();
  }

  underlyingTransport = createTransporter();
  createClientService();
}

void ThriftTransporter::createClientService(bool callRegistration) {

  std::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(new apache::thrift::protocol::TCompactProtocol(underlyingTransport));

  try{
    server->close();

    server->initialize(protocolPtr, callRegistration);
  }catch(...){

    auto suspectedVersion = cclient::data::InstanceVersion::getVersion(getConnection()->getHost()) + 1;
    logging::LOG_DEBUG(logger) << "Attempting API version " << suspectedVersion;
    switch (suspectedVersion) {
        case 1:
          server = std::make_unique<AccumuloServerFacadeV1>();
          break;
        case 2:
          server = std::make_unique<AccumuloServerFacadeV2>();
          break;
      }
    server->initialize(protocolPtr, callRegistration);
    // if successful, we'll update the map
    cclient::data::InstanceVersion::setVersion(getConnection()->getHost(),suspectedVersion);
    switchInterconnect();
    logging::LOG_DEBUG(logger) << "Successfully switch to API version " << suspectedVersion;
  }

}

void ThriftTransporter::newTransporter(const std::shared_ptr<ServerConnection> &conn) {

  clonedConnection = conn;

  std::shared_ptr<apache::thrift::transport::TSocket> serverTransport(new apache::thrift::transport::TSocket(conn->getHost(), conn->getPort()));

  std::shared_ptr<apache::thrift::transport::TTransport> transporty(new apache::thrift::transport::TFramedTransport(serverTransport));

  try {
    logging::LOG_TRACE(logger) << "attempting to connect to " << conn->getHost() << " and " << conn->getPort();
    transporty->open();

    logging::LOG_TRACE(logger) << "connected! to " << conn->getHost() << " and " << conn->getPort();
  } catch (const apache::thrift::transport::TTransportException &te) {
    logging::LOG_TRACE(logger) << conn->getHost() << " host-port " << conn->getPort() << te.what() << " ";
    try {
      transporty->close();
    } catch (const apache::thrift::transport::TTransportException &to) {
      logging::LOG_TRACE(logger) << conn->getHost() << " host-port " << conn->getPort() << te.what() << " ";
    }
    throw te;
  }

  underlyingTransport = transporty;

}

std::shared_ptr<apache::thrift::transport::TFramedTransport> ThriftTransporter::createTransporter() {
  auto serverTransport = std::make_shared<apache::thrift::transport::TSocket>(clonedConnection->getHost(), clonedConnection->getPort());

  serverTransport->setLinger(false, 0);
  serverTransport->setNoDelay(true);
  serverTransport->setConnTimeout(0);

  auto newTransport = std::make_shared<apache::thrift::transport::TFramedTransport>(serverTransport);

  newTransport->open();

  return newTransport;

}

}
