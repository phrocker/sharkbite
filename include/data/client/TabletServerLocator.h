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
#include "ExtentLocator.h"
#include "../exceptions/ClientException.h"
#include "../constructs/client/Instance.h"
#include "TabletLocationObtainer.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"

namespace cclient {
namespace impl {

struct locationComparator
{
  bool operator()(const std::string &left, const std::string &right) const{
    if (left.empty())
      return false;
    if (right.empty())
      return true;
    return left < right;
  }
};
/**
 * Mechanism to locate tablet servers.
 * Design: Implements tablet locator, which is a pure virtual class.
 **/
class TabletServerLocator : public TabletLocator {
 public:
  TabletServerLocator(std::string tableId, TabletLocator *parent, TabletLocationObtainer *locator, cclient::data::Instance *inst);
  virtual ~TabletServerLocator();

  /**
   * provides tablet locations by finding the begin and end row which are the metadata rows for this
   * table.
   **/
  virtual std::vector<cclient::data::TabletLocation> locations(cclient::data::security::AuthInfo *credentials) {

    std::stringstream metadataRow;

    metadataRow << tableId << ';';

    cclient::data::TabletLocation location = parent->locateTablet(credentials, metadataRow.str(), false, true);

    std::vector<cclient::data::TabletLocation> locations = locator->findTablet(credentials, &location, metadataRow.str(), lastTabletRow, parent);
    return locations;
  }

  /**
   * Locates tablets using the row as the begin row.
   * @param creds connecting user's credentials
   * @param row begin row
   * @param skipRow determines if the row can be skipped
   * @param retry determines if failures should be retried.
   **/
  cclient::data::TabletLocation locateTablet(cclient::data::security::AuthInfo *creds, std::string row, bool skipRow, bool retry) {

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

    retry_loop: try {
      cclient::data::TabletLocation parentLocation = parent->locateTablet(creds, metadataRow.str(), false, retry);

      std::vector<cclient::data::TabletLocation> locations = locator->findTablet(creds, &parentLocation, metadataRow.str(), lastTabletRow, parent);

      cclient::data::TabletLocation returnLocation;
      logging::LOG_DEBUG(logger) << tableId << " Received " << locations.size() << " locations";
      for (auto location : locations) {
        logging::LOG_DEBUG(logger) << tableId << " Received " << location.getLocation() << " " << location.getExtent();
        if (location.getExtent()->getPrevEndRow().length() == 0 || location.getExtent()->getPrevEndRow() < modifiedRow) {
          returnLocation = location;
          logging::LOG_DEBUG(logger) << tableId << " Received " << returnLocation.getLocation() << " " << returnLocation.getExtent();
          break;
        }
      }

      if (returnLocation != nullptr) {
        auto cachedRow = returnLocation.getExtent()->getEndRow();

        std::lock_guard<std::recursive_mutex> lock(locatorMutex);
        logging::LOG_DEBUG(logger) << tableId << " : " << "Caching " << cachedRow << " in the cache ";
        cachedLocations.insert(std::pair<std::string, cclient::data::TabletLocation>(cachedRow, returnLocation));

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

  inline void binMutations(cclient::data::security::AuthInfo *credentials, std::vector<std::shared_ptr<cclient::data::Mutation>> *mutations,
                           std::map<std::string, std::shared_ptr<cclient::data::TabletServerMutations>> *binnedMutations, std::set<std::string> *locations,
                           std::vector<std::shared_ptr<cclient::data::Mutation>> *failures) {
    std::map<std::string, std::shared_ptr<cclient::data::TabletServerMutations>>::iterator it;

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
        tsm = std::make_shared<cclient::data::TabletServerMutations>(loc.getSession());
        binnedMutations->insert(std::make_pair(loc.getLocation(), tsm));
      }

      tsm->addMutation(*loc.getExtent(), m);

    }
  }

  std::vector<cclient::data::Range*> binRanges(
      cclient::data::security::AuthInfo *credentials, std::vector<cclient::data::Range*> *ranges, std::set<std::string> *locations,
      std::map<std::string, std::map<std::shared_ptr<cclient::data::KeyExtent>, std::vector<cclient::data::Range*>, pointer_comparator<std::shared_ptr<cclient::data::KeyExtent>> > > *binnedRanges) {

    std::string startRow = "";
    std::vector<cclient::data::Range*> failures;
    std::vector<cclient::data::TabletLocation> tabletLocations;
    for (auto range : *ranges) {
      if (range->getStartKey() != NULL) {
        startRow = std::string(range->getStartKey()->getRow().first, range->getStartKey()->getRow().second);
      }

      cclient::data::TabletLocation loc;

      try {
        if (!getCachedLocation(startRow, loc)){
          loc = locateTablet(credentials, startRow, false, false);
          logging::LOG_DEBUG(logger) << "Received un-cached location " << loc.getLocation() << " " << loc.getExtent();
        }
      } catch (const cclient::exceptions::ClientException &ce) {
        failures.push_back(range);
        continue;
      }

      tabletLocations.push_back(loc);
      std::string stopKey = "";
      if (range->getStopKey() != NULL)
        stopKey = std::string(range->getStopKey()->getRow().first, range->getStopKey()->getRow().second);
      std::string extentEndRow = loc.getExtent()->getEndRow();

      while (!range->getInfiniteStopKey() && stopKey >= extentEndRow) {

        bool isCached = getCachedLocation(startRow, loc);
        if (!isCached){
          logging::LOG_DEBUG(logger) << "Received un-cached location " << loc.getLocation() << " " << loc.getExtent()<< " iscached? " << (isCached==true);
          loc = locateTablet(credentials, extentEndRow, true, false);
        }else{
          logging::LOG_DEBUG(logger) << "Received cached location " << loc.getLocation() << " " << loc.getExtent() << " iscached? " << (isCached==true);
        }


        tabletLocations.push_back(loc);

        extentEndRow = loc.getExtent()->getEndRow();

        if (extentEndRow.length() == 0)
          break;

        char *backing = new char[extentEndRow.length() + 1];
        memset(backing, 0x01, extentEndRow.length() + 1);
        memcpy(backing, extentEndRow.c_str(), extentEndRow.length());
        startRow = std::string(backing);
        delete [] backing;

        logging::LOG_DEBUG(logger) << "Changing start row to " << startRow;
      }
      for (auto locs : tabletLocations) {
        locations->insert(locs.getLocation());
        (*binnedRanges)[locs.getLocation()][locs.getExtent()].push_back(range);
      }

    }
    return failures;
  }

  void invalidateCache(cclient::data::KeyExtent failedExtent) {

    std::lock_guard<std::recursive_mutex> lock(locatorMutex);
    cachedLocations.erase(failedExtent.getEndRow());
  }

  void invalidateCache() {
    std::lock_guard<std::recursive_mutex> lock(locatorMutex);
    logging::LOG_DEBUG(logger) << "Invalidating the cache";
    cachedLocations.clear();
  }

  void invalidateCache(std::vector<cclient::data::KeyExtent> keySet) {
  }

 protected:
  std::string lastTabletRow;
  std::string tableId;
  TabletLocator *parent;
  TabletLocationObtainer *locator;
  std::map<std::string, cclient::data::TabletLocation, locationComparator> cachedLocations;
  std::recursive_mutex locatorMutex;

  cclient::data::Instance *instance;

  bool getCachedLocation(std::string startRow, cclient::data::TabletLocation &loc) {
    std::lock_guard<std::recursive_mutex> lock(locatorMutex);
    std::map<std::string, cclient::data::TabletLocation>::iterator it = cachedLocations.lower_bound(startRow);
    if (it != cachedLocations.end()) {
      loc = it->second;

      if (loc.getExtent()->getPrevEndRow().length() == 0 || loc.getExtent()->getPrevEndRow() < startRow) {
        logging::LOG_DEBUG(logger) << tableId  << " : " << startRow  << " , pendrow: " << loc.getExtent()->getPrevEndRow() << " endrow: " << loc.getExtent()->getEndRow() << " is cached out of  " << cachedLocations.size();
        return true;
      }
    }

    logging::LOG_DEBUG(logger) << tableId  << " : " << startRow << " is not cached out of  " << cachedLocations.size() << " returning false";
    return false;
  }

 private:
  std::shared_ptr<logging::Logger> logger;
};

} /* namespace data */
} /* namespace cclient */
#endif /* TABLETSERVERLOCATOR_H_ */
