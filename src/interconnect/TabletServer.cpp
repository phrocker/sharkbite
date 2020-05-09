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
#include <future>
#include <sys/socket.h>
#include <arpa/inet.h>

#include "interconnect/TabletServer.h"
#include "data/constructs/rfile/RFileOperations.h"
#include "data/streaming/accumulo/StreamSeekable.h"

namespace interconnect {

ServerInterconnect::ServerInterconnect(std::shared_ptr<cclient::data::tserver::RangeDefinition> rangeDef, const cclient::impl::Configuration *conf,
                                       TransportPool<ThriftTransporter> *distributedConnector)
    :
    logger(logging::LoggerFactory<ServerInterconnect>::getLogger()) {
  ConnectorService conn("tserver", rangeDef->getServer(), rangeDef->getPort());

  const uint16_t tserverPort = rangeDef->getPort();

  if (!isValidPort(tserverPort)) {
    throw cclient::exceptions::IllegalArgumentException("Invalid port");
  }

  const uint32_t timeout = conf->getLong(GENERAL_RPC_TIMEOUT_OPT,
  GENERAL_RPC_TIMEOUT);

  tServer = std::make_shared<ServerConnection>(conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT), rangeDef->getPort(), timeout);

  int failures = 0;
  do {

    try {
      myTransport = distributedConnector->getTransporter(tServer);
    } catch (const apache::thrift::transport::TTransportException &te) {

      logging::LOG_DEBUG(logger) << "Exception while getting transporter to " << conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT) << " " << rangeDef->getPort() << te.what();

      tServer = std::make_shared<ServerConnection>(conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT), rangeDef->getPort(), timeout);

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

ServerInterconnect::ServerInterconnect(std::shared_ptr<cclient::data::tserver::ServerDefinition> rangeDef, const cclient::impl::Configuration *conf,
                                       TransportPool<ThriftTransporter> *distributedConnector) {
  ConnectorService conn("tserver", rangeDef->getServer(), rangeDef->getPort());

  const uint16_t tserverPort = (uint16_t) conf->getLong(TSERVER_PORT_OPT,
  TSERVER_DEFAULT_PORT);

  if (!isValidPort(tserverPort)) {
    throw cclient::exceptions::IllegalArgumentException("Invalid port");
  }

  const uint32_t timeout = conf->getLong(GENERAL_RPC_TIMEOUT_OPT,
  GENERAL_RPC_TIMEOUT);

  tServer = std::make_shared<ServerConnection>(conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT), rangeDef->getPort(), timeout);
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

ServerInterconnect::ServerInterconnect(const std::string host, const int port, const cclient::impl::Configuration *conf, TransportPool<ThriftTransporter> *distributedConnector)
    :
    logger(logging::LoggerFactory<ServerInterconnect>::getLogger()),
    AccumuloConnector<interconnect::ThriftTransporter>(host, port) {
  ConnectorService conn("tserver", host, port);

  const uint16_t tserverPort = (uint16_t) conf->getLong(TSERVER_PORT_OPT,
  TSERVER_DEFAULT_PORT);

  if (!isValidPort(tserverPort)) {
    throw cclient::exceptions::IllegalArgumentException("Invalid port");
  }

  const uint32_t timeout = conf->getLong(GENERAL_RPC_TIMEOUT_OPT,
  GENERAL_RPC_TIMEOUT);

  tServer = std::make_shared<ServerConnection>(conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT), rangeDef->getPort(), timeout);

  myTransport = distributedConnector->getTransporter(tServer);
  setTransport(myTransport->getTransporter());

  authenticate(rangeDef->getCredentials());

  std::vector<cclient::data::IterInfo*> list;

  std::map<std::string, std::map<std::string, std::string>> map;

}

ServerInterconnect::~ServerInterconnect() {
  myTransportPool->freeTransport(myTransport);
}

Scan* ServerInterconnect::hedgedScan(std::shared_ptr<interconnect::ScanArbiter> &arbiter, std::atomic<bool> *isRunning, const std::vector<cclient::data::Column> &cols,
                                     const std::vector<cclient::data::IterInfo> &serverSideIterators, uint32_t batchSize) {
  ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>>> request(&credentials, rangeDef->getAuthorizations(), tServer);

  request.setBufferSize(batchSize);

  request.addColumns(cols);

  request.setIters(serverSideIterators);

  size_t rangeSize=0;

  for (std::shared_ptr<cclient::data::KeyExtent> extent : *rangeDef->getExtents()) {
    auto locs = extent->getFileLocations();

    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> *ident = new ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
        std::shared_ptr<cclient::data::Range>>();
    auto rangeSize = rangeDef->getRanges()->size();
    if (rangeSize == 0) {
      return NULL;
    }

    for (const auto range : *rangeDef->getRanges()) {
      ident->putIdentifier(extent, range);
      rangeSize++;
    }

    request.putIdentifier(ident);
  }

  if (request.size() > 1 || rangeSize > 1 || !cols.empty()) {
    std::cout << "not doing a single scan" << std::endl;
    return transport->beginScan(isRunning, &request);
  } else {
    std::cout << "doing a single scan" << std::endl;
    auto result0 = std::async([&] {

      ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> *ident = request.getRangeIdentifiers()->at(0);
      std::shared_ptr<cclient::data::KeyExtent> extent = ident->getGlobalMapping().at(0);
      auto locations = extent->getFileLocations();
      auto range = ident->getIdentifiers(extent).at(0);
      auto auths =rangeDef->getAuthorizations();
      for(auto loc : locations){
        std::cout << "opening " << loc << std::endl;
      }
      auto multi_iter = cclient::data::RFileOperations::openManySequential(locations);
      std::vector<std::string> cols;
      cclient::data::streams::StreamSeekable seekable(*range,cols,*auths,false);
      multi_iter->relocate(&seekable);
      int count=0;
      std::vector<std::shared_ptr<cclient::data::KeyValue> > res;
      Scan *newScan = new Scan(isRunning);
      while (multi_iter->hasNext() && isRunning) {
        auto top = multi_iter->getTop();
        res.emplace_back(top);
        multi_iter->next();

        if (++count >= 1000) {
          newScan->setHasMore(true);
          break;
        }

      }
      std::cout << "got " << count << " from rfile " << multi_iter->getEntriesFiltered() << std::endl;
      newScan->setMultiIterator(multi_iter);
      newScan->setRFileScan(true);
      newScan->setNextResults(&res);

      arbiter->add(newScan);
      return newScan;
    });

    auto result1 = std::async([&] {
      auto r = transport->beginScan(isRunning,&request);
      arbiter->add(r);
      return r;
    });
    return arbiter->wait();
  }
}

void ServerInterconnect::recreateConnection(bool errorOcurred) {
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

Scan*
ServerInterconnect::scan(std::atomic<bool> *isRunning, const std::vector<cclient::data::Column> &cols, const std::vector<cclient::data::IterInfo> &serverSideIterators, uint32_t batchSize) {
  ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>>> request(&credentials, rangeDef->getAuthorizations(), tServer);

  request.setBufferSize(batchSize);

  request.addColumns(cols);

  request.setIters(serverSideIterators);

  for (std::shared_ptr<cclient::data::KeyExtent> extent : *rangeDef->getExtents()) {
    auto locs = extent->getFileLocations();

    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>, std::shared_ptr<cclient::data::Range>> *ident = new ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
        std::shared_ptr<cclient::data::Range>>();
    if (rangeDef->getRanges()->size() == 0) {
      return NULL;
    }
    for (const auto range : *rangeDef->getRanges()) {
      ident->putIdentifier(extent, range);
    }

    request.putIdentifier(ident);
  }
  return transport->beginScan(isRunning, &request);

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

Scan* ServerInterconnect::scan(std::atomic<bool> *isRunning) {

  std::vector<cclient::data::Column> emptyCols;

  std::vector<cclient::data::IterInfo> emptyServerSideIterators;

  return scan(isRunning, emptyCols, emptyServerSideIterators);

}

Scan* ServerInterconnect::continueScan(Scan *scan) {
  if (scan->getHasMore() && scan->isClientRunning()) {
    if (scan->isRFileScan()) {
      auto multi_iter = scan->getMultiIterator();
      std::vector<std::shared_ptr<cclient::data::KeyValue> > res;
      int count = 0;
      while (multi_iter->hasNext() && scan->isClientRunning()) {
        auto top = multi_iter->getTop();

        multi_iter->next();

        if (++count >= 1000) {
          scan->setHasMore(true);
          break;
        }

      }
      scan->setNextResults(&res);
      return scan;
    } else {
      return transport->continueScan(scan);
    }
  }
  return nullptr;
}

std::shared_ptr<cclient::data::TabletServerMutations> ServerInterconnect::write(std::shared_ptr<cclient::data::TabletServerMutations> mutations) {

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

}
