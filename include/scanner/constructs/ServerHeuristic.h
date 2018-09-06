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
#include "../../data/client/ExtentLocator.h"
#include "../../data/extern/thrift/tabletserver_types.h"
#include "../../interconnect/Scan.h"
#include "../Source.h"

#include <thread>
#include <vector>
#include <mutex>

namespace scanners {

template<typename T>
struct ScanPair {
  Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *src;
  Heuristic<T> *heuristic;

};

/**
 * Contains base functionality to support multi scanning
 */

class ScannerHeuristic : Heuristic<interconnect::ThriftTransporter> {

 public:

  /**
   * Add a server interconnect
   */
  void addClientInterface(std::shared_ptr<interconnect::ClientInterface<interconnect::ThriftTransporter>> serverIfc) {
    std::lock_guard<std::mutex> lock(serverLock);
    Heuristic::addClientInterface(serverIfc);
  }

  explicit ScannerHeuristic(short numThreads = 10)
      : threadCount(numThreads),
        started(false) {

  }

  ~ScannerHeuristic() {
    std::lock_guard<std::mutex> lock(serverLock);

    if (started) {
      for (std::vector<std::thread>::iterator iter = threads.begin(); iter != threads.end(); iter++) {
        iter->join();
      }
    }
    started = false;

  }

  uint16_t scan(Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *source) {
    std::lock_guard<std::mutex> lock(serverLock);
    if (!started)
      started = true;
    uint16_t scans = 0;
    for (int i = 0; i < threadCount; i++) {
      ScanPair<interconnect::ThriftTransporter> *pair = new ScanPair<interconnect::ThriftTransporter>;
      pair->src = source;
      pair->heuristic = this;
      threads.push_back(std::thread(ScannerHeuristic::scanRoutine, pair));
    }
    return scans;
  }

 private:
  std::mutex serverLock;
  std::vector<std::thread> threads;
  uint16_t threadCount;
 protected:

  static void closeScan(Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *source) {

    source->getResultSet()->decrementProducers();

  }

  void addFailedScan(ScanPair<interconnect::ThriftTransporter> *scanResource, std::shared_ptr<interconnect::ServerInterconnect> server, interconnect::Scan *scan) {

    auto rangeDef = server->getRangesDefinition();
    std::shared_ptr<cclient::data::Key> lastKey = 0;
    if (NULL != scan)
      lastKey = scan->getTopKey();
    std::vector<cclient::data::Range*> *ranges = rangeDef->getRanges();
    std::vector<cclient::data::Range*> newRanges;
    for (auto range : *ranges) {
      if (NULL != lastKey && (range->getStopKey()) <= lastKey) {
        // skip entirely
        delete range;
      } else if (NULL != lastKey && (range->getStartKey()) <= lastKey) {
        cclient::data::Range *newRange = new cclient::data::Range(lastKey, false, range->getStopKey(), range->getStopKeyInclusive());

        // create a new range
        newRanges.push_back(newRange);
      } else {
        newRanges.push_back(range);
      }
    }

    std::vector<std::shared_ptr<cclient::data::tserver::RangeDefinition>> locatedTablets;

    scanResource->src->locateFailedTablet(newRanges, &locatedTablets);

    for (auto newRangeDef : locatedTablets) {
      auto directConnect = std::make_shared<interconnect::ServerInterconnect>(newRangeDef, scanResource->src->getInstance()->getConfiguration());

      ((ScannerHeuristic*) scanResource->heuristic)->addClientInterface(directConnect);
    }

  }

  static void *
  scanRoutine(ScanPair<interconnect::ThriftTransporter> *scanResource) {

    Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>> *source = scanResource->src;

    source->getResultSet()->registerProducer();

    std::shared_ptr<interconnect::ServerInterconnect> conn = 0;
    do {
      conn = ((ScannerHeuristic*) scanResource->heuristic)->next();

      interconnect::Scan *scan = 0;
      if (NULL != conn) {

        try {
          scan = conn->scan(source->getColumns(), source->getIters());

          do {
            std::vector<std::shared_ptr<cclient::data::KeyValue> > nextResults;

            scan->getNextResults(&nextResults);

            source->getResultSet()->add_ptr(&nextResults);
            nextResults.clear();

            interconnect::Scan *newScan = conn->continueScan(scan);

            if (NULL == newScan) {
              delete scan;
              scan = NULL;
            } else
              scan = newScan;

          } while (scan != NULL);
        } catch (org::apache::accumulo::core::tabletserver::thrift::NotServingTabletException &te) {

          ((ScannerHeuristic*) scanResource->heuristic)->addFailedScan(scanResource, conn, scan);
        }

      } else {
        delete scanResource;
        break;
      }
    } while (NULL != conn);

    closeScan(source);

    return 0;

  }

  virtual std::shared_ptr<interconnect::ServerInterconnect> next() {
    std::shared_ptr<interconnect::ClientInterface<interconnect::ThriftTransporter>> nextService = NULL;

    std::lock_guard<std::mutex> lock(serverLock);

    if (!servers.empty()) {
      nextService = servers.back();
      servers.pop_back();
    }

    std::shared_ptr<interconnect::ServerInterconnect> connector = std::dynamic_pointer_cast<interconnect::ServerInterconnect>(nextService);

    return connector;

  }

  volatile bool started;
  std::vector<interconnect::ClientInterface<interconnect::ThriftTransporter>*>::iterator it;

};
}
#endif /* SERVERHEURISTIC_H_ */
