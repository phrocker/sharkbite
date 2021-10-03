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

#ifndef TABLETSERVERLOCATOR_H_
#define TABLETSERVERLOCATOR_H_

#include <map>
#include <mutex>
#include <sstream>

#include "../constructs/client/Instance.h"
#include "../exceptions/ClientException.h"
#include "ExtentLocator.h"
#include "TabletLocationObtainer.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#include "utils/StringUtils.h"

namespace cclient {
namespace impl {

struct locationComparator {
  bool operator()(const std::string &left, const std::string &right) const {
    if (left.empty()) return false;
    if (right.empty()) return true;
    return left < right;
  }
};
/**
 * Mechanism to locate tablet servers.
 * Design: Implements tablet locator, which is a pure virtual class.
 **/
class TabletServerLocator : public TabletLocator {
 public:
  TabletServerLocator(std::string tableId, TabletLocator *parent,
                      TabletLocationObtainer *locator,
                      std::shared_ptr<cclient::data::Instance> inst);
  virtual ~TabletServerLocator();

  /**
   * provides tablet locations by finding the begin and end row which are the
   *metadata rows for this table.
   **/
  virtual std::vector<cclient::data::TabletLocation> locations(
      cclient::data::security::AuthInfo *credentials) override{
    std::stringstream metadataRow;

    metadataRow << tableId << ';';

    cclient::data::TabletLocation location =
        parent->locateTablet(credentials, metadataRow.str(), false, true);

    std::vector<cclient::data::TabletLocation> locations = locator->findTablet(
        credentials, &location, metadataRow.str(), lastTabletRow, parent);
    return locations;
  }

  /**
   * Locates tablets using the row as the begin row.
   * @param creds connecting user's credentials
   * @param row begin row
   * @param skipRow determines if the row can be skipped
   * @param retry determines if failures should be retried.
   **/
  cclient::data::TabletLocation locateTablet(
      cclient::data::security::AuthInfo *creds, const std::string &row, bool skipRow,
      bool retry) override{
    std::string modifiedRow;

    if (skipRow) {
      char *backing = new char[row.length() + 1];
      memset(backing, 0x01, row.length() + 1);
      memcpy(backing, row.c_str(), row.length());
      modifiedRow = std::string(backing);
      delete[] backing;
    } else
      modifiedRow = row;

    // check cached

    std::stringstream metadataRow;

    metadataRow << tableId << ';' << modifiedRow;

    logging::LOG_DEBUG(logger) << "searching for " << metadataRow.str();

  retry_loop:
    try {
      cclient::data::TabletLocation parentLocation =
          parent->locateTablet(creds, metadataRow.str(), false, retry);

      std::vector<cclient::data::TabletLocation> locations =
          locator->findTablet(creds, &parentLocation, metadataRow.str(),
                              lastTabletRow, parent);

      cclient::data::TabletLocation returnLocation;
      logging::LOG_DEBUG(logger) << tableId << " locateTablet Received "
                                 << locations.size() << " locations";
      std::lock_guard<std::recursive_mutex> lock(locatorMutex);
      for (auto location : locations) {
        logging::LOG_DEBUG(logger)
            << tableId << " locateTablet Received " << location.getLocation()
            << " " << location.getExtent();
        auto cachedRow = location.getExtent()->getEndRow();
        if (cachedRow.empty()) {
          cachedRow = "\uffff\uffff\uffff";
        }
        logging::LOG_DEBUG(logger)
            << tableId << " : "
            << "Caching " << cachedRow << " in the cache ";
        cachedLocations.insert(
            std::pair<std::string, cclient::data::TabletLocation>(cachedRow,
                                                                  location));
      }

      if (getCachedLocation(modifiedRow, returnLocation)) {
        return returnLocation;
      } else {
        if (retry)
          goto retry_loop;
        else
          throw cclient::exceptions::ClientException(NO_LOCATION_IDENTIFIED);
      }
    } catch (const cclient::exceptions::ClientException &ce) {
      if (retry)
        goto retry_loop;
      else
        throw cclient::exceptions::ClientException(NO_LOCATION_IDENTIFIED);
    }

    throw cclient::exceptions::ClientException(NO_LOCATION_IDENTIFIED);
  }

  inline void binMutations(
      cclient::data::security::AuthInfo *credentials,
      std::vector<std::shared_ptr<cclient::data::Mutation>> *mutations,
      std::map<std::string,
               std::shared_ptr<cclient::data::TabletServerMutations>>
          *binnedMutations,
      std::set<std::string> *locations,
      std::vector<std::shared_ptr<cclient::data::Mutation>> *failures) override{
    std::map<std::string,
             std::shared_ptr<cclient::data::TabletServerMutations>>::iterator
        it;

    for (const auto &m : *mutations) {
      cclient::data::TabletLocation loc;

      if (!getCachedLocation(m->getRow(), loc))
        loc = locateTablet(credentials, m->getRow(), false, false);

      std::shared_ptr<cclient::data::TabletServerMutations> tsm = nullptr;
      it = binnedMutations->find(loc.getLocation());
      if (it != binnedMutations->end()) {
        tsm = it->second;
      }

      if (NULL == tsm) {
        locations->insert(loc.getLocation());
        tsm = std::make_shared<cclient::data::TabletServerMutations>(
            loc.getSession());
        binnedMutations->insert(std::make_pair(loc.getLocation(), tsm));
      }

      tsm->addMutation(*loc.getExtent(), m);
    }
  }

  std::vector<std::shared_ptr<cclient::data::Range>> binRanges(
      cclient::data::security::AuthInfo *credentials,
      std::vector<std::shared_ptr<cclient::data::Range>> *ranges,
      std::set<std::string> *locations,
      std::map<std::string,
               std::map<std::shared_ptr<cclient::data::KeyExtent>,
                        std::vector<std::shared_ptr<cclient::data::Range>>,
                        pointer_comparator<
                            std::shared_ptr<cclient::data::KeyExtent>>>>
          *binnedRanges) override{
    std::string startRow = "";
    std::vector<std::shared_ptr<cclient::data::Range>> failures;

    for (auto range : *ranges) {
      std::string stopKey = "";
      std::vector<cclient::data::TabletLocation> tabletLocations;
      if (range->getStartKey() != NULL) {
        startRow = std::string(range->getStartKey()->getRow().first,
                               range->getStartKey()->getRow().second);
      }
      if (range->getStopKey() != NULL)
        stopKey = std::string(range->getStopKey()->getRow().first,
                              range->getStopKey()->getRow().second);

      logging::LOG_DEBUG(logger) << startRow << " and stop " << stopKey;
      cclient::data::TabletLocation loc;

      try {
        if (!getCachedLocation(startRow, loc)) {
          loc = locateTablet(credentials, startRow, false, false);
        }
      } catch (const cclient::exceptions::ClientException &ce) {
        failures.push_back(range);
        continue;
      }

      logging::LOG_DEBUG(logger) << startRow << " binRanges Received location "
                                 << loc.getLocation() << " " << loc.getExtent();
      tabletLocations.push_back(loc);

      std::string extentEndRow = loc.getExtent()->getEndRow();

      std::string lookupRow = extentEndRow;

      auto test = extentEndRow;
      test.append(1, 0x01);
      auto testKey = std::make_shared<cclient::data::Key>();
      testKey->setRow(test);
      uint32_t iterations = 0;
      while (!(loc.getExtent()->getEndRow() == "<" ||
               loc.getExtent()->getEndRow().empty()) &&
             (stopKey.empty() || test.compare(stopKey) <= 0)) {
        iterations++;
        logging::LOG_DEBUG(logger)
            << "start loop " << stopKey << " and start " << startRow;
        logging::LOG_DEBUG(logger) << "start loop "
                                   << " " << test;
        lookupRow.append(1, 0x01);
        bool isCached = getCachedLocation(lookupRow, loc);
        if (!isCached) {
          logging::LOG_DEBUG(logger)
              << lookupRow
              << "binRanges Received un-cached location, will locate "
              << loc.getLocation() << " " << loc.getExtent() << " "
              << extentEndRow << " iscached? " << (isCached == true);
          loc = locateTablet(credentials, extentEndRow, true, false);
        } else {
          logging::LOG_DEBUG(logger)
              << lookupRow << " binRanges Received cached location "
              << loc.getLocation() << " " << loc.getExtent() << " "
              << extentEndRow << " iscached? " << (isCached == true);
        }

        tabletLocations.push_back(loc);

        extentEndRow = loc.getExtent()->getEndRow();

        if (extentEndRow.length() == 0) break;

        lookupRow = extentEndRow;

        test = extentEndRow;
        test.append(1, 0x01);
        testKey = std::make_shared<cclient::data::Key>();
        testKey->setRow(test);

        logging::LOG_DEBUG(logger)
            << "Changing start row to " << test << " from " << extentEndRow;
      }
      logging::LOG_DEBUG(logger)
          << "Executed " << std::to_string(iterations)
          << " iterations of the loop, resulting in "
          << std::to_string(tabletLocations.size()) << " locations";
      for (auto locs : tabletLocations) {
        locations->insert(locs.getLocation());
        (*binnedRanges)[locs.getLocation()][locs.getExtent()].push_back(range);
      }
    }
    return failures;
  }

  void invalidateCache(cclient::data::KeyExtent failedExtent) override{
    std::lock_guard<std::recursive_mutex> lock(locatorMutex);
    cachedLocations.erase(failedExtent.getEndRow());
  }

  void invalidateCache() override{
    std::lock_guard<std::recursive_mutex> lock(locatorMutex);
    logging::LOG_DEBUG(logger) << "Invalidating the cache";
    cachedLocations.clear();
  }

  void invalidateCache(std::vector<cclient::data::KeyExtent> keySet) override {}

 protected:
  std::string lastTabletRow;
  std::string tableId;
  TabletLocator *parent;
  TabletLocationObtainer *locator;
  std::map<std::string, cclient::data::TabletLocation, locationComparator>
      cachedLocations;
  std::recursive_mutex locatorMutex;

  std::shared_ptr<cclient::data::Instance> instance;

  bool getCachedLocation(std::string startRow,
                         cclient::data::TabletLocation &loc) {
    std::lock_guard<std::recursive_mutex> lock(locatorMutex);
    std::map<std::string, cclient::data::TabletLocation>::iterator it =
        startRow.empty() ? cachedLocations.begin()
                         : cachedLocations.lower_bound(startRow);
    if (it != cachedLocations.end()) {
      loc = it->second;
      logging::LOG_DEBUG(logger)
          << "Checking " << tableId << " : " << startRow
          << " , pendrow: " << loc.getExtent()->getPrevEndRow()
          << " endrow: " << loc.getExtent()->getEndRow()
          << " is cached out of  " << cachedLocations.size();
      if (loc.getExtent()->getPrevEndRow().length() == 0 ||
          loc.getExtent()->getPrevEndRow() < startRow) {
        logging::LOG_DEBUG(logger)
            << tableId << " : " << startRow
            << " , pendrow: " << loc.getExtent()->getPrevEndRow()
            << " endrow: " << loc.getExtent()->getEndRow()
            << " is cached out of  " << cachedLocations.size();
        return true;
      }
    } else {
      logging::LOG_DEBUG(logger)
          << "Nothing found for " << utils::StringUtils::hex_ascii(startRow);
    }

    logging::LOG_DEBUG(logger)
        << tableId << " : " << startRow << " is not cached out of  "
        << cachedLocations.size() << " returning false";
    return false;
  }

 private:
  std::shared_ptr<logging::Logger> logger;
};

}  // namespace impl
} /* namespace cclient */
#endif /* TABLETSERVERLOCATOR_H_ */
