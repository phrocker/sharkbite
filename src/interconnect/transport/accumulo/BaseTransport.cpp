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

ThriftTransporter::ThriftTransporter(const std::shared_ptr<ServerConnection> &conn, bool reg)
    :
    interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, std::shared_ptr<cclient::data::Mutation>>(conn),
    logger(logging::LoggerFactory < ThriftTransporter > ::getLogger()) {
  auto suspectedVersion = cclient::data::InstanceVersion::getVersion(conn->toString());
  logging::LOG_DEBUG(logger) << "Attempting API version " << (suspectedVersion == -1 ? 1 : suspectedVersion) << " for " << conn->toString();
  switch (suspectedVersion) {
    case -1: {
      if (reg) {
        createClientService(true);
      } else {
        server = std::make_unique<AccumuloServerFacadeV1>();
      }
      break;
    }
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

  logging::LOG_DEBUG(logger) << "Attempting registration on " << getConnection()->toString() << " with version " << std::to_string(cclient::data::InstanceVersion::getVersion(getConnection()->toString()))
                             << " attempting reg ? " << (callRegistration == true);
  // need to get instance information if the host is not cached
  if (cclient::data::InstanceVersion::isDefined(getConnection()->toString()) && callRegistration)
    callRegistration = false;
  try {
    server->close();
    logging::LOG_DEBUG(logger) << "Attempting registration on " << getConnection()->toString() << " with version " << std::to_string(cclient::data::InstanceVersion::getVersion(getConnection()->toString()))
                               << " attempting reg ? " << (callRegistration == true);
    server->initialize(protocolPtr, callRegistration);
    if (callRegistration) {
      auto suspectedVersion = cclient::data::InstanceVersion::getVersion(getConnection()->toString());
      cclient::data::InstanceVersion::setVersion(getConnection()->toString(), suspectedVersion);
    }
  } catch (...) {
    auto eptr = std::current_exception();  // capture
    auto suspectedVersion = cclient::data::InstanceVersion::getVersion(getConnection()->toString()) + 1;
    std::stringstream msg;
    msg << clonedConnection->getHost() << ":" << clonedConnection->getPort();
    printUncaughtException(eptr, msg.str());
    logging::LOG_DEBUG(logger) << (suspectedVersion - 1) << " did not work, attempting API version " << suspectedVersion;
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
    server->initialize(protocolPtr, callRegistration);
    // if successful, we'll update the map
    if (callRegistration)
      cclient::data::InstanceVersion::setVersion(getConnection()->toString(), suspectedVersion);
    switchInterconnect();
    logging::LOG_DEBUG(logger) << "Successfully switch to API version " << suspectedVersion;
  }

  logging::LOG_DEBUG(logger) << getConnection()->toString() << " with version " << cclient::data::InstanceVersion::getVersion(getConnection()->toString());
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
