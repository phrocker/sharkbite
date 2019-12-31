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

#include <memory>
#include <string>

#include <sys/socket.h>
#include <arpa/inet.h>

#include "interconnect/TabletServer.h"

namespace interconnect {

ServerInterconnect::ServerInterconnect(const std::string host, const int port, const cclient::impl::Configuration *conf, TransportPool<ThriftTransporter> *distributedConnector)
    : logger(logging::LoggerFactory < ServerInterconnect > ::getLogger()), AccumuloConnector<interconnect::ThriftTransporter>(host, port) {
  ConnectorService conn("tserver", host, port);

  const uint16_t tserverPort = (uint16_t) conf->getLong(TSERVER_PORT_OPT,
  TSERVER_DEFAULT_PORT);

  if (!isValidPort(tserverPort)) {
    throw cclient::exceptions::IllegalArgumentException("Invalid port");
  }

  const uint32_t timeout = conf->getLong(GENERAL_RPC_TIMEOUT_OPT,
  GENERAL_RPC_TIMEOUT);

  tServer = std::make_shared < ServerConnection > (conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT), rangeDef->getPort(), timeout);

  myTransport = distributedConnector->getTransporter(tServer);
  setTransport(myTransport->getTransporter());

  authenticate(rangeDef->getCredentials());

  std::vector<cclient::data::IterInfo*> list;

  std::map<std::string, std::map<std::string, std::string>> map;

}

ServerInterconnect::~ServerInterconnect() {
  myTransportPool->freeTransport(myTransport);
}

void ServerInterconnect::authenticate(cclient::data::security::AuthInfo *credentials) {
  bool success = false;
  do {
    try {
      //transport->authenticate (credentials);
      this->credentials = *credentials;
      std::string username = credentials->getUserName();
      std::string password = credentials->getPassword();

      setCredentials(username, password);
      setAuthenticated(true);
      success = true;
    } catch (const apache::thrift::TApplicationException &ta) {
      recreateConnection(true);
    } catch (const apache::thrift::transport::TTransportException &te) {
      recreateConnection(true);
    } catch (const apache::thrift::protocol::TProtocolException &tp) {
      recreateConnection(true);
    }

  } while (!success);
  // need to return those that could not be written or those that failed.

}

}
