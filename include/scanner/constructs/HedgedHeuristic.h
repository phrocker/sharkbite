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

#ifndef HEDGEDHEURISTIC_H
#define HEDGEDHEURISTIC_H

#include <thrift/TApplicationException.h>

#include <chrono>
#include <mutex>
#include <thread>
#include <vector>

#include "../../data/client/ExtentLocator.h"
#include "../../data/extern/thrift/tabletserver_types.h"
#include "../../interconnect/Scan.h"
#include "../../interconnect/ScanArbiter.h"
#include "../../interconnect/TabletServer.h"
#include "../Source.h"
#include "ServerHeuristic.h"
#include "data/constructs/IterInfo.h"
#include "data/exceptions/NotServingException.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace scanners {

/**
 * Contains base functionality to support multi scanning
 */

class HedgedScannerHeuristic : public ScannerHeuristic {
 private:
  std::shared_ptr<logging::Logger> logger;

  std::vector<cclient::data::IterInfo> iters;

 public:
  explicit HedgedScannerHeuristic(short numThreads = 10)
      : ScannerHeuristic(numThreads),
        logger(logging::LoggerFactory<HedgedScannerHeuristic>::getLogger()) {}

  ~HedgedScannerHeuristic() { close(); }

  virtual void close() override;

  virtual uint16_t scan(
      Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>>
          *source) override;

  void setTableIterators(std::vector<cclient::data::IterInfo> iters);

 protected:
  virtual std::shared_ptr<logging::Logger> getLogger() override {
    return logger;
  }

  static void *hedgedScan(
      ScanPair<interconnect::ThriftTransporter> *scanResource) {
    Source<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>>
        *source = scanResource->src;

    cclient::data::IterInfo versioningIterator;
    if (scanResource->ownedAdditionalFeatures) {
      std::vector<cclient::data::IterInfo> *iterators =
          static_cast<std::vector<cclient::data::IterInfo> *>(
              scanResource->ownedAdditionalFeatures);
      for (const auto &iterator : *iterators) {
        if (iterator.getClass() ==
            "org.apache.accumulo.core.iterators.user.VersioningIterator") {
          versioningIterator = iterator;
        }
      }
    }

    source->getResultSet()->registerProducer();

    std::shared_ptr<interconnect::ServerInterconnect> conn = 0;

    bool failed = false;
    std::shared_ptr<interconnect::ScanArbiter> arbiter =
        std::make_shared<interconnect::ScanArbiter>(1,
                                                    scanResource->disableRpc);
    do {
      conn = ((HedgedScannerHeuristic *)scanResource->heuristic)->next();

      failed = false;
      interconnect::Scan *scan = 0;
      if (NULL != conn) {
        try {
          scan = conn->hedgedScan(arbiter, scanResource->runningFlag,
                                  source->getColumns(), source->getIters(),
                                  versioningIterator, 1000,
                                  scanResource->disableRpc);

          do {
            if (!scanResource->runningFlag->load()) {
              break;
            }

            if (scan->isRFileScan()) {
              logging::LOG_TRACE(
                  ((HedgedScannerHeuristic *)scanResource->heuristic)
                      ->getLogger())
                  << "RFile scan completed first";
            } else {
              logging::LOG_TRACE(
                  ((HedgedScannerHeuristic *)scanResource->heuristic)
                      ->getLogger())
                  << "Accumulo scan completed first";
            }

            std::vector<std::shared_ptr<cclient::data::KeyValue>> nextResults;

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
          ((HedgedScannerHeuristic *)scanResource->heuristic)
              ->addFailedScan(scanResource, conn, scan);
        } catch (const org::apache::accumulov2::core::tabletserver::thrift::
                     NoSuchScanIDException &te) {
          if (scanResource->runningFlag->load()) {
            throw te;
          }
        } catch (const cclient::exceptions::NotServingException &te) {
          logging::LOG_TRACE(
              ((HedgedScannerHeuristic *)scanResource->heuristic)->getLogger())
              << "Not serving " << te.what();
          ((HedgedScannerHeuristic *)scanResource->heuristic)
              ->addFailedScan(scanResource, conn, scan);

          conn.reset();

          failed = true;

          continue;
        }

      } else {
        logging::LOG_TRACE(
            ((HedgedScannerHeuristic *)scanResource->heuristic)->getLogger())
            << "connection is null";
        delete scanResource;
        break;
      }
    } while (failed || (NULL != conn && scanResource->runningFlag->load()));

    closeScan(source);

    return 0;
  }
};
}  // namespace scanners
#endif /* SERVERHEURISTIC_H_ */
