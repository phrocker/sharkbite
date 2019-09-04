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

#ifndef TABLETSERVER_H_
#define TABLETSERVER_H_ 1

#ifndef SIGNED_RIGHT_SHIFT_IS
#define SIGNED_RIGHT_SHIFT_IS 5
#endif

#ifndef ARITHMETIC_RIGHT_SHIFT
#define ARITHMETIC_RIGHT_SHIFT 5
#endif

#include <chrono>
#include <thread>
#include <string>
#include "InterConnect.h"
#include "TransportPool.h"
#include "../data/constructs/inputvalidation.h"
#include "../data/constructs/IterInfo.h"
#include "../data/constructs/configuration/Configuration.h"
#include "../data/extern/thrift/data_types.h"
#include "../data/constructs/scanstate.h"
#include "../data/exceptions/ClientException.h"
#include "../data/exceptions/NotServingException.h"
#include "../data/exceptions/IllegalArgumentException.h"
#include "../data/constructs/tablet/TabletType.h"
#include "../data/constructs/client/TabletServerMutations.h"
#include "../interconnect/ClientInterface.h"

#include "transport/ServerTransport.h"
#include "transport/BaseTransport.h"
#include "TransportPool.h"
#include "AccumuloConnector.h"
#include "../data/constructs/server/RangeDefinition.h"
#include "../data/constructs/server/ServerDefinition.h"
#include "../data/extern/thrift/ClientService.h"
#include "../data/extern/thrift/TabletClientService.h"

namespace interconnect {

static TransportPool<ThriftTransporter> CLUSTER_COORDINATOR;

extern void closeAllThriftConnections();

class ServerInterconnect : public AccumuloConnector<interconnect::ThriftTransporter> {

 protected:
  ServerInterconnect(cclient::data::security::AuthInfo *creds, TransportPool<ThriftTransporter> *distributedConnector = &CLUSTER_COORDINATOR) {
    myTransportPool = distributedConnector;
    this->credentials = *creds;
    myTransport = NULL;
  }
  std::shared_ptr<CachedTransport<ThriftTransporter>> myTransport;

  void recreateConnection(bool errorOcurred = false) {
    do {
      myTransport->sawError(errorOcurred);
      try {
        myTransportPool->freeTransport(myTransport);
      } catch (const apache::thrift::transport::TTransportException &te) {
        // close may occur on a partial write this is okay
        // to know
      }

      try {
        myTransport = myTransportPool->getTransporter(tServer);
        setTransport(myTransport->getTransporter());
        break;

      } catch (const apache::thrift::protocol::TProtocolException &tpe) {
        myTransport->sawError(true);
        myTransportPool->freeTransport(myTransport);
        std::this_thread::sleep_for(std::chrono::milliseconds(50));
        continue;
      } catch (const apache::thrift::transport::TTransportException &tpe) {
        myTransport->sawError(true);
        myTransportPool->freeTransport(myTransport);
        std::this_thread::sleep_for(std::chrono::milliseconds(50));
        continue;
      }
      break;
    } while (true);
  }

 public:

  ServerInterconnect(const std::string host, const int port, const cclient::impl::Configuration *conf, TransportPool<ThriftTransporter> *distributedConnector = &CLUSTER_COORDINATOR);

  ServerInterconnect(std::shared_ptr<cclient::data::tserver::RangeDefinition> rangeDef, const cclient::impl::Configuration *conf, TransportPool<ThriftTransporter> *distributedConnector =
                         &CLUSTER_COORDINATOR) {
    ConnectorService conn("tserver", rangeDef->getServer(), rangeDef->getPort());

    const uint16_t tserverPort = (uint16_t) conf->getLong(TSERVER_PORT_OPT,
    TSERVER_DEFAULT_PORT);

    if (!isValidPort(tserverPort)) {
      throw cclient::exceptions::IllegalArgumentException("Invalid port");
    }

    const uint32_t timeout = conf->getLong(GENERAL_RPC_TIMEOUT_OPT,
    GENERAL_RPC_TIMEOUT);

    tServer = std::make_shared < ServerConnection > (conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT), rangeDef->getPort(), timeout);

    int failures = 0;
    do {

      try {
        myTransport = distributedConnector->getTransporter(tServer);
      } catch (const apache::thrift::transport::TTransportException &te) {
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
        // close may occur on a partial write this is okay
        // to know
        if (++failures > 2)
          throw te;
        continue;
      }

      try {

        setTransport(myTransport->getTransporter());
        break;

      } catch (const apache::thrift::protocol::TProtocolException &tpe) {
        myTransport->sawError(true);
        if (++failures > 2)
          throw tpe;
        distributedConnector->freeTransport(myTransport);
        continue;
      } catch (const apache::thrift::transport::TTransportException &tpe) {
        myTransport->sawError(true);
        distributedConnector->freeTransport(myTransport);
        if (++failures > 2)
          throw tpe;
        continue;
      }

      break;
    } while (true);

    myTransportPool = distributedConnector;

    authenticate(rangeDef->getCredentials());

    std::vector<cclient::data::IterInfo*> list;

    std::map<std::string, std::map<std::string, std::string>> map;

    this->rangeDef = rangeDef;

  }

  Scan *
  scan(std::vector<cclient::data::Column*> *cols, std::vector<cclient::data::IterInfo*> *serverSideIterators) {
    ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*>> request(&credentials, rangeDef->getAuthorizations(), tServer);

    request.addColumns(cols);

    request.setIters(serverSideIterators);

    for (std::shared_ptr<cclient::data::KeyExtent> extent : *rangeDef->getExtents()) {
      ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*> *ident = new ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, cclient::data::Range*>();
      if (rangeDef->getRanges()->size() == 0) {
        return NULL;
      }
      for (cclient::data::Range *range : *rangeDef->getRanges()) {
        ident->putIdentifier(extent, range);
      }

      request.putIdentifier(ident);
    }

    return transport->beginScan(&request);

  }

  ServerInterconnect(std::shared_ptr<cclient::data::tserver::ServerDefinition> rangeDef, const cclient::impl::Configuration *conf, TransportPool<ThriftTransporter> *distributedConnector =
                         &CLUSTER_COORDINATOR) {
    ConnectorService conn("tserver", rangeDef->getServer(), rangeDef->getPort());

    const uint16_t tserverPort = (uint16_t) conf->getLong(TSERVER_PORT_OPT,
    TSERVER_DEFAULT_PORT);

    if (!isValidPort(tserverPort)) {
      throw cclient::exceptions::IllegalArgumentException("Invalid port");
    }

    const uint32_t timeout = conf->getLong(GENERAL_RPC_TIMEOUT_OPT,
    GENERAL_RPC_TIMEOUT);

    tServer = std::make_shared < ServerConnection > (conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT), rangeDef->getPort(), timeout);
    do {

      try {
        myTransport = distributedConnector->getTransporter(tServer);
      } catch (const apache::thrift::transport::TTransportException &te) {
        myTransport->sawError(true);
        distributedConnector->freeTransport(myTransport);
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
        // close may occur on a partial write this is okay
        // to know
        continue;
      }

      try {

        setTransport(myTransport->getTransporter());
        break;

      } catch (const apache::thrift::protocol::TProtocolException &tpe) {
        myTransport->sawError(true);
        distributedConnector->freeTransport(myTransport);
        continue;
      } catch (const apache::thrift::transport::TTransportException &tpe) {
        myTransport->sawError(true);
        distributedConnector->freeTransport(myTransport);
        continue;
      }

      break;
    } while (true);

    myTransportPool = distributedConnector;

    authenticate(rangeDef->getCredentials());

    std::vector<cclient::data::IterInfo*> list;
  }

  Scan *scan() {

    std::vector<cclient::data::Column*> emptyCols;

    std::vector<cclient::data::IterInfo*> emptyServerSideIterators;

    return scan(&emptyCols, &emptyServerSideIterators);

  }

  Scan *continueScan(Scan *scan) {

    if (scan->getHasMore()) {
      try {
        return transport->continueScan(scan);
      } catch (org::apache::accumulo::core::tabletserver::thrift::NotServingTabletException &te) {
        throw cclient::exceptions::NotServingException(te.what());
      }
    }
    return NULL;
  }

  std::shared_ptr<cclient::data::TabletServerMutations> write(std::shared_ptr<cclient::data::TabletServerMutations> mutations) {

    bool success = false;
    uint32_t failures = 0;
    do {
      try {
        transport->write(&credentials, mutations->getMutations());
        success = true;
      } catch (const apache::thrift::transport::TTransportException &te) {
        if (++failures > mutations->getMaxFailures())
          return mutations;
        recreateConnection(true);
      } catch (const apache::thrift::protocol::TProtocolException &tp) {
        if (++failures > mutations->getMaxFailures())
          return mutations;
        recreateConnection(true);
      }

    } while (!success);
    // need to return those that could not be written or those that failed.
    return NULL;
  }

  void halt() {

  }

  void
  authenticate(cclient::data::security::AuthInfo *credentials);

  virtual void authenticate(const std::string &username, const std::string &password) override {
  }

  virtual
  ~ServerInterconnect();

};

}
#endif /* TABLETSERVER_H_ */

