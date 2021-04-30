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

#include "interconnect/TabletServer.h"

#include <arpa/inet.h>
#include <sys/socket.h>

#include <future>
#include <memory>
#include <string>

#include "data/constructs/rfile/RFileOperations.h"
#include "data/streaming/accumulo/StreamSeekable.h"

namespace interconnect {

ServerInterconnect::ServerInterconnect(
    std::shared_ptr<cclient::data::tserver::RangeDefinition> rangeDef,
    const cclient::impl::Configuration *conf,
    TransportPool<ThriftTransporter> *distributedConnector)
    : logger(logging::LoggerFactory<ServerInterconnect>::getLogger()) {
  ConnectorService conn("tserver", rangeDef->getServer(), rangeDef->getPort());

  const uint16_t tserverPort = rangeDef->getPort();

  if (!isValidPort(tserverPort)) {
    throw cclient::exceptions::IllegalArgumentException("Invalid port");
  }

  const uint32_t timeout =
      conf->getLong(GENERAL_RPC_TIMEOUT_OPT, GENERAL_RPC_TIMEOUT);

  tServer = std::make_shared<ServerConnection>(
      conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT),
      rangeDef->getPort(), timeout);

  /*
  int failures = 0;
  do {

    try {
      myTransport = distributedConnector->getTransporter(tServer);
    } catch (const apache::thrift::transport::TTransportException &te) {

      logging::LOG_DEBUG(logger) << "Exception while getting transporter to " <<
  conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT) << " "
  << rangeDef->getPort() << te.what();

      tServer =
  std::make_shared<ServerConnection>(conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT),
  rangeDef->getPort(), timeout);

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
  } while (true);*/
  myTransport = nullptr;

  myTransportPool = distributedConnector;

  authenticate(rangeDef->getCredentials());

  std::vector<cclient::data::IterInfo *> list;

  std::map<std::string, std::map<std::string, std::string>> map;

  this->rangeDef = rangeDef;
}

ServerInterconnect::ServerInterconnect(
    std::shared_ptr<cclient::data::tserver::ServerDefinition> rangeDef,
    const cclient::impl::Configuration *conf,
    TransportPool<ThriftTransporter> *distributedConnector) {
  ConnectorService conn("tserver", rangeDef->getServer(), rangeDef->getPort());

  const uint16_t tserverPort =
      (uint16_t)conf->getLong(TSERVER_PORT_OPT, TSERVER_DEFAULT_PORT);

  if (!isValidPort(tserverPort)) {
    throw cclient::exceptions::IllegalArgumentException("Invalid port");
  }

  const uint32_t timeout =
      conf->getLong(GENERAL_RPC_TIMEOUT_OPT, GENERAL_RPC_TIMEOUT);

  tServer = std::make_shared<ServerConnection>(
      conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT),
      rangeDef->getPort(), timeout);

  /*
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
*/
  myTransport = nullptr;
  myTransportPool = distributedConnector;

  authenticate(rangeDef->getCredentials());

  std::vector<cclient::data::IterInfo *> list;
}

ServerInterconnect::ServerInterconnect(
    const std::string host, const int port,
    const cclient::impl::Configuration *conf,
    TransportPool<ThriftTransporter> *distributedConnector)
    : logger(logging::LoggerFactory<ServerInterconnect>::getLogger()),
      AccumuloBaseConnector<interconnect::ThriftTransporter>(host, port) {
  ConnectorService conn("tserver", host, port);

  const uint16_t tserverPort =
      (uint16_t)conf->getLong(TSERVER_PORT_OPT, TSERVER_DEFAULT_PORT);

  if (!isValidPort(tserverPort)) {
    throw cclient::exceptions::IllegalArgumentException("Invalid port");
  }

  const uint32_t timeout =
      conf->getLong(GENERAL_RPC_TIMEOUT_OPT, GENERAL_RPC_TIMEOUT);

  tServer = std::make_shared<ServerConnection>(
      conn.getAddressString(interconnect::INTERCONNECT_TYPES::TSERV_CLIENT),
      rangeDef->getPort(), timeout);

  myTransport = distributedConnector->getTransporter(tServer);
  setTransport(myTransport->getTransporter());

  authenticate(rangeDef->getCredentials());

  std::vector<cclient::data::IterInfo *> list;

  std::map<std::string, std::map<std::string, std::string>> map;
}

ServerInterconnect::~ServerInterconnect() {
  if (myTransport) myTransportPool->freeTransport(myTransport);
}

Scan *ServerInterconnect::hedgedScan(
    std::shared_ptr<interconnect::ScanArbiter> &arbiter,
    std::atomic<bool> *isRunning,
    const std::vector<cclient::data::Column> &cols,
    const std::vector<cclient::data::IterInfo> &serverSideIterators,
    cclient::data::IterInfo &versioningIterator, uint32_t batchSize,
    bool disableRpc) {
  if (!myTransport) {
    recreateConnection();
  }
  ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                             std::shared_ptr<cclient::data::Range>>>
      request(&credentials, rangeDef->getAuthorizations(), tServer);

  ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                             std::shared_ptr<cclient::data::Range>>>
      hedgedRequest(&credentials, rangeDef->getAuthorizations(), tServer);

  request.setBufferSize(batchSize);

  request.addColumns(cols);

  request.setIters(serverSideIterators);

  hedgedRequest.setBufferSize(batchSize);

  hedgedRequest.addColumns(cols);

  hedgedRequest.setIters(serverSideIterators);

  size_t rangeSize = 0;

  for (std::shared_ptr<cclient::data::KeyExtent> extent :
       *rangeDef->getExtents()) {
    auto locs = extent->getFileLocations();

    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                   std::shared_ptr<cclient::data::Range>> *ident =
        new ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                           std::shared_ptr<cclient::data::Range>>();

    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                   std::shared_ptr<cclient::data::Range>> *hedgedident =
        new ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                           std::shared_ptr<cclient::data::Range>>();
    auto rangeSize = rangeDef->getRanges()->size();
    if (rangeSize == 0) {
      return NULL;
    }

    for (const auto range : *rangeDef->getRanges()) {
      ident->putIdentifier(extent, range);
      hedgedident->putIdentifier(extent, range);
      rangeSize++;
    }

    request.putIdentifier(ident);
    hedgedRequest.putIdentifier(hedgedident);
  }

  if (request.size() > 1 || rangeSize > 1 || !cols.empty()) {
    logging::LOG_DEBUG(logger) << "Initiating a non-hedged read";
    return transport->beginScan(isRunning, &request);
  } else {
    logging::LOG_DEBUG(logger)
        << "Initiating a hedged read on" << rangeSize << " ranges";
    auto result0 = std::async([&] {
      try {
        ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                       std::shared_ptr<cclient::data::Range>> *ident =
            hedgedRequest.getRangeIdentifiers()->at(0);
        std::shared_ptr<cclient::data::KeyExtent> extent =
            ident->getGlobalMapping().at(0);
        auto locations = extent->getFileLocations();
        auto range = ident->getIdentifiers(extent).at(0);
        auto auths = rangeDef->getAuthorizations();
        int maxVersion = 0;
        if (!versioningIterator.empty()) {
          try {
            auto maxvs = versioningIterator.getOption("maxVersion", "0");
            logging::LOG_DEBUG(logger) << "Max versions is " << maxvs;
            maxVersion = std::stoi(maxvs);
          } catch (...) {
          }
        } else {
          logging::LOG_DEBUG(logger) << "Max versions is empty";
        }
        auto multi_iter = cclient::data::RFileOperations::openManySequential(
            locations, maxVersion);
        std::vector<std::string> cols;
        auto seekable = std::make_shared<cclient::data::streams::StreamSeekable>(*range, cols, *auths,
                                                        false);
        multi_iter->relocate(seekable);
        int count = 0;
        std::vector<std::shared_ptr<cclient::data::KeyValue>> res;
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
        newScan->setMultiIterator(multi_iter);
        newScan->setRFileScan(true);
        newScan->setNextResults(&res);

        arbiter->add(newScan);
        return newScan;
      } catch (const cclient::exceptions::ClientException &e) {
        auto r = new Scan(isRunning);
        r->setRFileScan(true);
        logging::LOG_DEBUG(logger)
            << "Client exception whilst scanning, " << e.what();
        r->setException(e.what());
        arbiter->add(r);
        return r;
      } catch (...) {
        auto r = new Scan(isRunning);
        r->setRFileScan(true);
        auto eptr = std::current_exception();
        try {
          std::rethrow_exception(eptr);
        } catch (const std::exception &e) {
          logging::LOG_DEBUG(logger)
              << "Exception whilst scanning, " << e.what();
          r->setException(e.what());
        } catch (...) {
          logging::LOG_DEBUG(logger) << "Unkonwn exception while scanning";
          r->setException("Unknown Exception");
        }

        arbiter->add(r);
        return r;
      }
    });
    if (!disableRpc) {
      auto result1 = std::async([&] {
        try {
          auto r = transport->beginScan(isRunning, &request);
          arbiter->add(r);
          return r;
        } catch (...) {
          try {
            // try again
            auto r = transport->beginScan(isRunning, &request);
            arbiter->add(r);
          } catch (...) {
            auto r = new Scan(isRunning);
            r->setMultiScan(true);
            auto eptr = std::current_exception();
            try {
              std::rethrow_exception(eptr);
            } catch (const std::exception &e) {
              r->setException(e.what());
            } catch (...) {
              r->setException("Unknown Exception");
            }

            arbiter->add(r);
            return r;
          }
        }
        return (Scan *)nullptr;
      });
    }
    return arbiter->wait();
  }
}

void ServerInterconnect::recreateConnection(bool errorOcurred) {
  do {
    if (myTransport) myTransport->sawError(errorOcurred);
    try {
      if (myTransport) myTransportPool->freeTransport(myTransport);
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

Scan *ServerInterconnect::scan(
    std::atomic<bool> *isRunning,
    const std::vector<cclient::data::Column> &cols,
    const std::vector<cclient::data::IterInfo> &serverSideIterators,
    uint32_t batchSize) {
  if (!myTransport) {
    recreateConnection();
  }
  ScanRequest<ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                             std::shared_ptr<cclient::data::Range>>>
      request(&credentials, rangeDef->getAuthorizations(), tServer);

  request.setBufferSize(batchSize);

  request.addColumns(cols);

  request.setIters(serverSideIterators);

  for (std::shared_ptr<cclient::data::KeyExtent> extent :
       *rangeDef->getExtents()) {
    auto locs = extent->getFileLocations();

    ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
                   std::shared_ptr<cclient::data::Range>> *ident =
        new ScanIdentifier<std::shared_ptr<cclient::data::KeyExtent>,
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

void ServerInterconnect::authenticate(
    cclient::data::security::AuthInfo *credentials) {
  bool success = false;
  do {
    try {
      // transport->authenticate (credentials);
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

Scan *ServerInterconnect::scan(std::atomic<bool> *isRunning) {
  if (!myTransport) {
    recreateConnection();
  }
  std::vector<cclient::data::Column> emptyCols;

  std::vector<cclient::data::IterInfo> emptyServerSideIterators;

  return scan(isRunning, emptyCols, emptyServerSideIterators);
}

Scan *ServerInterconnect::continueScan(Scan *scan) {
  if (!myTransport) {
    recreateConnection();
  }
  if (scan->getHasMore() && scan->isClientRunning()) {
    if (scan->isRFileScan()) {
      auto multi_iter = scan->getMultiIterator();
      std::vector<std::shared_ptr<cclient::data::KeyValue>> res;
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

std::shared_ptr<cclient::data::TabletServerMutations> ServerInterconnect::write(
    std::shared_ptr<cclient::data::TabletServerMutations> mutations) {
  if (!myTransport) {
    recreateConnection();
  }
  bool success = false;
  uint32_t failures = 0;
  do {
    try {
      transport->write(&credentials, mutations->getMutations());
      success = true;
    } catch (const apache::thrift::transport::TTransportException &te) {
      if (++failures > mutations->getMaxFailures()) return mutations;
      recreateConnection(true);
    } catch (const apache::thrift::protocol::TProtocolException &tp) {
      if (++failures > mutations->getMaxFailures()) return mutations;
      recreateConnection(true);
    }

  } while (!success);
  // need to return those that could not be written or those that failed.
  return NULL;
}

}  // namespace interconnect
