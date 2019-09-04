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

namespace interconnect {

ThriftTransporter::ThriftTransporter(const std::shared_ptr<ServerConnection> &conn)
    : interconnect::ServerTransport<apache::thrift::transport::TTransport, cclient::data::KeyExtent, cclient::data::Range*, cclient::data::Mutation*>(conn) {

  newTransporter(conn);
}

ThriftTransporter::~ThriftTransporter() {
  underlyingTransport->close();
}

std::map<std::string, std::string> ThriftTransporter::getNamespaceConfiguration(cclient::data::security::AuthInfo *auth, const std::string &nameSpaceName) {
  return server.getNamespaceConfiguration(auth, nameSpaceName);
}

apache::thrift::transport::TTransport ThriftTransporter::getTransport() {
  return *underlyingTransport;
}

void ThriftTransporter::authenticate(cclient::data::security::AuthInfo *auth) {
  server.authenticate(auth);
}

void ThriftTransporter::createIfClosed() {
  if (underlyingTransport.get() != NULL && !underlyingTransport->isOpen()) {

    underlyingTransport = boost::tools::from_shared_ptr<apache::thrift::transport::TTransport>(createTransporter());
    createClientService();
  }
}

void ThriftTransporter::closeAndCreateClient() {
  if (underlyingTransport.get() != NULL && underlyingTransport->isOpen()) {
    underlyingTransport->close();
  }

  underlyingTransport = boost::tools::from_shared_ptr<apache::thrift::transport::TTransport>(createTransporter());
  createClientService();
}

void ThriftTransporter::createClientService() {

  boost::shared_ptr<apache::thrift::protocol::TProtocol> protocolPtr(new apache::thrift::protocol::TCompactProtocol(underlyingTransport));

  server.close();

  server.initialize(protocolPtr);

}

void ThriftTransporter::newTransporter(const std::shared_ptr<ServerConnection> &conn) {

  clonedConnection = conn;

  boost::shared_ptr<apache::thrift::transport::TSocket> serverTransport(new apache::thrift::transport::TSocket(conn->getHost(), conn->getPort()));

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

std::shared_ptr<apache::thrift::transport::TFramedTransport> ThriftTransporter::createTransporter() {
  auto serverTransport = std::make_shared<apache::thrift::transport::TSocket>(clonedConnection->getHost(), clonedConnection->getPort());

  serverTransport->setLinger(false, 0);
  serverTransport->setNoDelay(true);
  serverTransport->setConnTimeout(0);

  auto newTransport = std::make_shared<apache::thrift::transport::TFramedTransport>(boost::tools::from_shared_ptr<apache::thrift::transport::TSocket>(serverTransport));

  newTransport->open();

  return newTransport;

}

}
