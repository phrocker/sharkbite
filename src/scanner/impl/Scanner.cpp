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

#include "scanner/impl/Scanner.h"

#include <algorithm>
#include <map>
#include <set>
#include <string>

#include "data/client/ExtentLocator.h"
#include "data/client/LocatorCache.h"
#include "data/constructs/Key.h"
#include "data/constructs/client/zookeeperinstance.h"
#include "data/constructs/inputvalidation.h"
#include "data/constructs/security/AuthInfo.h"
#include "data/constructs/security/Authorizations.h"
#include "data/constructs/value.h"
#include "interconnect/ClientInterface.h"
#include "interconnect/tableOps/TableOperations.h"
#include "scanner/Source.h"
#include "scanner/constructs/Results.h"
#include "scanner/constructs/ServerHeuristic.h"
#include "utils/StringUtils.h"

namespace scanners {

Scanner::Scanner(
    std::shared_ptr<cclient::data::Instance> instance,
    interconnect::TableOperations<cclient::data::KeyValue,
                                  ResultBlock<cclient::data::KeyValue>> *tops,
    cclient::data::security::Authorizations *auths, uint16_t threads)
    : scannerAuths(auths),
      numThreads(threads),
      logger(logging::LoggerFactory<Scanner>::getLogger()) {
  connectorInstance =
      dynamic_pointer_cast<cclient::data::zookeeper::ZookeeperInstance>(
          instance);
  if (connectorInstance == nullptr) {
    logging::LOG_ERROR(logger) << "Connector instance is an unexpected type";
    throw std::runtime_error("Connector instance is an unexpected type");
  }
  resultSet = NULL;
  tableLocator = cclient::impl::cachedLocators.getLocator(
      cclient::impl::LocatorKey(connectorInstance, tops->getTableId()));
  scannerHeuristic = std::make_unique<ScannerHeuristic>(numThreads);
  credentials = tops->getCredentials();
}

void Scanner::addRange(const cclient::data::Range &range) {
  auto newRange = std::make_shared<cclient::data::Range>(
      range.getStartKey(), range.getStartKeyInclusive(), range.getStopKey(),
      range.getStopKeyInclusive(), false);
  logging::LOG_TRACE(logger)
      << "passing in ranges " << range << " " << *newRange.get();
  ranges.push_back(newRange);
}

/**
 * Adds a range to the scanner
 * @param range
 **/
void Scanner::addRange(std::unique_ptr<cclient::data::Range> range) {
  std::lock_guard<std::mutex> lock(scannerLock);
  // we are now the owner
  std::shared_ptr<cclient::data::Range> sharedRange = std::move(range);
  ranges.push_back(sharedRange);
}

Results<cclient::data::KeyValue, ResultBlock<cclient::data::KeyValue>>
    *Scanner::getResultSet() {
  std::lock_guard<std::mutex> lock(scannerLock);

  if (IsEmpty(&ranges)) {
    throw cclient::exceptions::ClientException(RANGE_NOT_SPECIFIED);
  }
  if (IsEmpty(resultSet) && IsEmpty(&servers)) {
    for (const auto &range : ranges) {
      logging::LOG_TRACE(logger) << "range is " << *range.get();
    }

    resultSet = new Results<cclient::data::KeyValue,
                            ResultBlock<cclient::data::KeyValue>>();

    std::map<
        std::string,
        std::map<std::shared_ptr<cclient::data::KeyExtent>,
                 std::vector<std::shared_ptr<cclient::data::Range>>,
                 pointer_comparator<std::shared_ptr<cclient::data::KeyExtent>>>>
        returnRanges;
    std::set<std::string> locations;
    tableLocator->binRanges(credentials, &ranges, &locations, &returnRanges);

    for (std::string location : locations) {
      logging::LOG_TRACE(logger) << " Evaluating ranges for " << location;
      std::vector<std::string> locationSplit = split(location, ':');
      if (locationSplit.size() != 2) {
      }
      char *res = 0;
      errno = 0;
      uint64_t port = strtoul(locationSplit.at(1).c_str(), &res, 10);
      if (((port == (uint64_t)LONG_MIN || port == (uint64_t)LONG_MAX) &&
           errno != 0) ||
          *res != '\0') {
        throw cclient::exceptions::ClientException(INVALID_SERVER_PORT);
      }
      for (auto hostExtents : returnRanges.at(location)) {
        std::vector<std::shared_ptr<cclient::data::KeyExtent>> extents;
        for (const auto &rng : hostExtents.second) {
          logging::LOG_DEBUG(logger) << " extent is " << hostExtents.first;
          if (!rng->getInfiniteStartKey())
            logging::LOG_DEBUG(logger)
                << " extent is " << rng->getStartKey()->getRowStr();
        }
        extents.push_back(hostExtents.first);

        auto extentRange = hostExtents.first->toRange();
        // clip the ranges into the extents
        std::vector<std::shared_ptr<cclient::data::Range>> clippedRanges;

        for (const auto &range : hostExtents.second) {
          logging::LOG_DEBUG(logger)
              << " begin range is " << *extentRange.get();
          logging::LOG_DEBUG(logger) << " begin range is " << *range.get()
                                     << " " << *extentRange.get();
          auto rng = extentRange->intersect(range);
          if (nullptr == rng) {
            logging::LOG_DEBUG(logger)
                << " clipped range is null " << *range.get() << " "
                << *extentRange.get();
            clippedRanges.push_back(range);
          } else {
            logging::LOG_DEBUG(logger)
                << " clipped range is " << *rng.get() << " from "
                << *range.get() << " and " << *extentRange.get();
            clippedRanges.push_back(rng);
          }
        }

        logging::LOG_DEBUG(logger)
            << " clipped range is " << std::to_string(clippedRanges.size());

        auto rangeDef =
            std::make_shared<cclient::data::tserver::RangeDefinition>(
                credentials, scannerAuths, locationSplit.at(0), port,
                &clippedRanges, &extents, columns);

        std::shared_ptr<interconnect::ServerInterconnect> directConnect =
            std::make_shared<interconnect::ServerInterconnect>(
                rangeDef, connectorInstance->getConfiguration());
        scannerHeuristic->addClientInterface(directConnect);
      }
    }

    // begin the scan, however the pre-configured heuristic chooses how to do so
    scannerHeuristic->scan(this);
  }

  return resultSet;
}

void Scanner::locateFailedTablet(
    std::vector<std::shared_ptr<cclient::data::Range>> ranges,
    std::vector<std::shared_ptr<cclient::data::tserver::RangeDefinition>>
        *locatedTablets) {
  std::map<
      std::string,
      std::map<std::shared_ptr<cclient::data::KeyExtent>,
               std::vector<std::shared_ptr<cclient::data::Range>>,
               pointer_comparator<std::shared_ptr<cclient::data::KeyExtent>>>>
      returnRanges;
  std::set<std::string> locations;
  tableLocator->invalidateCache();
  tableLocator->binRanges(credentials, &ranges, &locations, &returnRanges);

  for (std::string location : locations) {
    std::vector<std::string> locationSplit = split(location, ':');
    if (locationSplit.size() != 2) {
    }
    char *res = 0;
    errno = 0;
    uint64_t port = strtoul(locationSplit.at(1).c_str(), &res, 10);
    if (((port == (uint64_t)LONG_MIN || port == (uint64_t)LONG_MAX) &&
         errno != 0) ||
        *res != '\0') {
      throw cclient::exceptions::ClientException(INVALID_SERVER_PORT);
    }
    for (auto hostExtents : returnRanges.at(location)) {
      std::vector<std::shared_ptr<cclient::data::KeyExtent>> extents;
      extents.push_back(hostExtents.first);
      auto rangeDef = std::make_shared<cclient::data::tserver::RangeDefinition>(
          credentials, scannerAuths, locationSplit.at(0), port,
          &hostExtents.second, &extents, columns);

      locatedTablets->push_back(rangeDef);
    }
  }
}

std::vector<cclient::data::IterInfo> Scanner::getTableIterators(
    std::string iterName) {
  std::vector<cclient::data::IterInfo> matches;
  std::string name, clazz;
  uint32_t priority;
  std::map<std::string, std::string> options;
  for (const auto &opt : tableOptions) {
    if (opt.first.rfind("table.iterator.scan", 0) == 0) {
      // found a scan time iterator.
      auto iterOptsName = utils::StringUtils::split(opt.first, ".");
      // table. iterator. scan . name . opt . optname
      if (iterOptsName.size() < 4) {
        continue;
      }
      if (name != iterOptsName.at(3)) {
        if (!name.empty() && !clazz.empty()) {
          matches.emplace_back(
              cclient::data::IterInfo(name, clazz, priority, options));
          name = iterOptsName.at(3);
          options.clear();
        }
      }
      if (utils::StringUtils::endsWith(opt.second, iterName)) {
        // have found your iterator.
        auto iterPriorityName = utils::StringUtils::split(opt.second, ",");
        if (iterPriorityName.size() == 2) {
          clazz = iterName;
          priority = std::atoi(iterPriorityName.at(0).c_str());
        }
      } else {
        if (iterOptsName.size() == 6) {
          options[iterOptsName[5]] = opt.second;
        }
      }
    }
  }
  if (!name.empty() && !clazz.empty()) {
    matches.emplace_back(
        cclient::data::IterInfo(name, clazz, priority, options));
    options.clear();
  }
  return matches;
}

void Scanner::setOption(ScannerOptions opt) {
  std::lock_guard<std::mutex> lock(scannerLock);

  if (opt == (opt & ScannerOptions::ENABLE_HEDGED_READS) ||
      opt == (opt & ScannerOptions::ENABLE_RFILE_SCANNER)) {
    /**
     * We are changing the scanner type amidst the requested option change
     */
    if (scannerHeuristic->isRunning()) {
      throw cclient::exceptions::ClientException(SCANNER_ALREADY_STARTED);
    }
    auto heuristic =
        std::make_unique<scanners::HedgedScannerHeuristic>(numThreads);
    /**
     * need to determine if the versioning iterator is used.
     */
    auto iterators = getTableIterators(
        "org.apache.accumulo.core.iterators.user.VersioningIterator");

    heuristic->setTableIterators(std::move(iterators));

    if (opt == (opt & ScannerOptions::ENABLE_RFILE_SCANNER)) {
      heuristic->disableRpcCalls();
    }

    scannerHeuristic = std::move(heuristic);
    sourceOptions |= ScannerOptions::ENABLE_HEDGED_READS;
  }
}

void Scanner::removeOption(ScannerOptions opt) {
  std::lock_guard<std::mutex> lock(scannerLock);
  if ((opt == (opt & ScannerOptions::ENABLE_HEDGED_READS)) &&
      (sourceOptions ==
       (sourceOptions & ScannerOptions::ENABLE_HEDGED_READS))) {
    /**
     * We are changing the scanner type amidst the requested option change
     */
    if (scannerHeuristic->isRunning()) {
      throw cclient::exceptions::ClientException(SCANNER_ALREADY_STARTED);
    }
    scannerHeuristic = std::make_unique<scanners::ScannerHeuristic>(numThreads);
    sourceOptions &= ~ScannerOptions::ENABLE_HEDGED_READS;
  }
}

}  // namespace scanners
