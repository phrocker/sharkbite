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

#ifndef SERVERHEURISTIC_H_
#define SERVERHEURISTIC_H_

#include "Heuristic.h"
#include "../../interconnect/TabletServer.h"
#include <thrift/TApplicationException.h>
#include "data/exceptions/NotServingException.h"
#include "../../data/client/ExtentLocator.h"
#include "../../data/extern/thrift/tabletserver_types.h"
#include "../../interconnect/Scan.h"
#include "../Source.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#include <thread>
#include <vector>
#include <chrono>
#include <mutex>

namespace scanners {

template<typename T>
struct ScanPair {
  Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *src;
  Heuristic<T> *heuristic;
  bool disableRpc;
  void *ownedAdditionalFeatures;
  std::atomic<bool> *runningFlag;
};

/**
 * Contains base functionality to support multi scanning
 */

class ScannerHeuristic : public Heuristic<interconnect::ThriftTransporter> {
 private:
  std::shared_ptr<logging::Logger> logger;

 protected:
 bool disableRpc;
 public:

  /**
   * Add a server interconnect
   */
  void addClientInterface(std::shared_ptr<interconnect::ClientInterface<interconnect::ThriftTransporter>> serverIfc) {
    std::lock_guard<std::timed_mutex> lock(serverLock);
    Heuristic::addClientInterface(serverIfc);
  }

  explicit ScannerHeuristic(short numThreads = 10)
      :
      logger(logging::LoggerFactory<ScannerHeuristic>::getLogger()),
      threadCount(numThreads),
      started(false),
      running(false),
      disableRpc(false){

  }

  ~ScannerHeuristic() {
    close();
  }

  bool isRunning() const {
    return running;
  }

void disableRpcCalls(){
    disableRpc = true;
  } 


  virtual void close() {
    running = false;
    std::lock_guard<std::timed_mutex> lock(serverLock);

    if (started) {
      for (std::vector<std::thread>::iterator iter = threads.begin(); iter != threads.end(); iter++) {
        iter->join();
      }
    }
    started = false;
  }

  virtual uint16_t scan(Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *source) {
    acquireLock();
    std::lock_guard<std::timed_mutex> lock(serverLock, std::adopt_lock);
    if (!started) {
      started = true;
      running = true;
    }
    uint16_t scans = 0;
    for (int i = 0; i < threadCount; i++) {
      ScanPair<interconnect::ThriftTransporter> *pair = new ScanPair<interconnect::ThriftTransporter>;
      pair->src = source;
      pair->heuristic = this;
      pair->runningFlag = &running;
      pair->disableRpc = disableRpc;
      pair->ownedAdditionalFeatures=nullptr;
      threads.push_back(std::thread(ScannerHeuristic::scanRoutine, pair));
    }
    return scans;
  }

 protected:
  std::timed_mutex serverLock;
  std::vector<std::thread> threads;
  uint16_t threadCount;
 protected:

  virtual std::shared_ptr<logging::Logger> getLogger()  {
    return logger;
  }

  inline bool acquireLock() {
    auto now = std::chrono::steady_clock::now();
    return serverLock.try_lock_until(now + std::chrono::seconds(1));
  }

  static void closeScan(Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *source) {

    source->getResultSet()->decrementProducers();

  }

  void addFailedScan(ScanPair<interconnect::ThriftTransporter> *scanResource, const std::shared_ptr<interconnect::ServerInterconnect> &server, interconnect::Scan *scan) {

    auto rangeDef = server->getRangesDefinition();
    logging::LOG_TRACE(logger) << "Adding failed scan";
    std::shared_ptr<cclient::data::Key> lastKey = 0;
    if (NULL != scan)
      lastKey = scan->getTopKey();
    auto ranges = rangeDef->getRanges();
    std::vector<std::shared_ptr<cclient::data::Range>> newRanges;
    if (NULL != lastKey) {
      logging::LOG_TRACE(logger) << "Have last key " << lastKey;
    }
    for (auto range : *ranges) {
      if (NULL != lastKey && (range->getStopKey()) <= lastKey) {
        // skip entirely

      } else if (NULL != lastKey && (range->getStartKey()) <= lastKey) {
        auto newRange = std::make_shared<cclient::data::Range>(lastKey, false, range->getStopKey(), range->getStopKeyInclusive());
        logging::LOG_TRACE(logger) << "Creating range " << newRange;
        // create a new range
        newRanges.push_back(newRange);
      } else {
        logging::LOG_TRACE(logger) << "Adding range " << range;
        newRanges.push_back(range);
      }
    }

    std::vector<std::shared_ptr<cclient::data::tserver::RangeDefinition>> locatedTablets;

    scanResource->src->locateFailedTablet(newRanges, &locatedTablets);

    logging::LOG_TRACE(logger) << "Adding failed scan " << locatedTablets.size();
    for (auto newRangeDef : locatedTablets) {
      auto directConnect = std::make_shared<interconnect::ServerInterconnect>(newRangeDef, scanResource->src->getInstance()->getConfiguration());

      addUniqueConnection(directConnect);
    }

  }

  static void* scanRoutine(ScanPair<interconnect::ThriftTransporter> *scanResource) {

    Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *source = scanResource->src;

    source->getResultSet()->registerProducer();

    std::shared_ptr<interconnect::ServerInterconnect> conn = 0;

    bool failed = false;
    do {
      conn = ((ScannerHeuristic*) scanResource->heuristic)->next();

      failed = false;
      interconnect::Scan *scan = 0;
      if (NULL != conn) {

        try {

          scan = conn->scan(scanResource->runningFlag, source->getColumns(), source->getIters());

          do {

            if (!scanResource->runningFlag->load()) {
              break;
            }

            std::vector<std::shared_ptr<cclient::data::KeyValue> > nextResults;

            scan->getNextResults(&nextResults);

            if (!nextResults.empty()) {
              source->getResultSet()->add_ptr(&nextResults);
              nextResults.clear();
            }

            if (!scanResource->runningFlag->load()) {
              break;
            }

            interconnect::Scan *newScan = conn->continueScan(scan);

            if (!scanResource->runningFlag->load()) {
              break;
            }

            if (NULL == newScan) {
              delete scan;
              scan = NULL;
            } else {
              if (newScan->getTopKey() == nullptr && scan->getTopKey()) {
                newScan->setTopKey(scan->getTopKey());
              }
              scan = newScan;
            }

          } while (scan != NULL);
        } catch (const apache::thrift::TApplicationException &te) {
          if (scanResource->runningFlag->load()) {
            throw te;
          }
          ((ScannerHeuristic*) scanResource->heuristic)->addFailedScan(scanResource, conn, scan);
        } catch (const org::apache::accumulov2::core::tabletserver::thrift::NoSuchScanIDException &te) {
          if (scanResource->runningFlag->load()) {
            throw te;
          }
        } catch (const cclient::exceptions::NotServingException &te) {
          logging::LOG_TRACE(((ScannerHeuristic*) scanResource->heuristic)->getLogger()) << "Not serving " << te.what();
          ((ScannerHeuristic*) scanResource->heuristic)->addFailedScan(scanResource, conn, scan);

          conn.reset();

          failed = true;

          continue;
        }

      } else {
        logging::LOG_TRACE(((ScannerHeuristic*) scanResource->heuristic)->getLogger()) << "connection is null";
        delete scanResource;
        break;
      }
    } while (failed || (NULL != conn && scanResource->runningFlag->load()));

    closeScan(source);

    return 0;

  }

  void addUniqueConnection(const std::shared_ptr<interconnect::ServerInterconnect> &rangeDef) {
    std::shared_ptr<interconnect::ClientInterface<interconnect::ThriftTransporter>> nextService = NULL;
    logging::LOG_TRACE(logger) << "addUniqueConnection Called";
    while (!acquireLock()) {
      if (!running) {
        logging::LOG_TRACE(logger) << "addUniqueConnection Called, but not running, returning";
        return;
      }
    }
    std::lock_guard<std::timed_mutex> lock(serverLock, std::adopt_lock);

    logging::LOG_TRACE(logger) << " addUniqueConnection Called, servers.size is " << std::to_string(servers.size());
    for (const auto &rd : servers) {
      logging::LOG_TRACE(logger) << "Testing range defs";
      if (*rangeDef->getRangesDefinition().get() == *std::dynamic_pointer_cast<interconnect::ServerInterconnect>(rd)->getRangesDefinition().get()) {
        logging::LOG_TRACE(logger) << "Range def already in server set. returning";
        return;

      }
    }
    servers.push_back(rangeDef);
    logging::LOG_TRACE(logger) << "addUniqueConnection Called, servers.size is " << std::to_string(servers.size());
  }

  virtual std::shared_ptr<interconnect::ServerInterconnect> next() {
    std::shared_ptr<interconnect::ClientInterface<interconnect::ThriftTransporter>> nextService = NULL;
    logging::LOG_TRACE(logger) << "Next Called";
    while (!acquireLock()) {
      if (!running) {
        logging::LOG_TRACE(logger) << "Next Called, but not running, returning nullptr";
        return nullptr;
      }
    }
    std::lock_guard<std::timed_mutex> lock(serverLock, std::adopt_lock);

    logging::LOG_TRACE(logger) << "Next Called, servers.size is " << std::to_string(servers.size());
    if (!servers.empty()) {
      nextService = servers.back();
      servers.pop_back();
    }
    logging::LOG_TRACE(logger) << "Next Called, servers.size is " << std::to_string(servers.size());

    std::shared_ptr<interconnect::ServerInterconnect> connector = std::dynamic_pointer_cast<interconnect::ServerInterconnect>(nextService);

    return connector;

  }

  std::atomic<bool> running;
  volatile bool started;
  std::vector<interconnect::ClientInterface<interconnect::ThriftTransporter>*>::iterator it;

};
}
#endif /* SERVERHEURISTIC_H_ */
