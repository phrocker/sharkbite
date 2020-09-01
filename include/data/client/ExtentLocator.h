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

#ifndef EXTENTLOCATOR_H_
#define EXTENTLOCATOR_H_

#include <string>
#include <map>
#include <vector>
#include <memory>
#include <set>
#include "../constructs/client/TabletServerMutations.h"
#include "../constructs/Mutation.h"
#include "../constructs/Range.h"
#include "../constructs/client/Instance.h"

#include "TabletLocation.h"
namespace cclient
{
  namespace impl
  {

    /**
 * Purpose: Provides a mechanism for caching locators
 * Design: Simply implements &lt; and &gt; functions. 
 **/
    class LocatorKey
    {
    public:
      LocatorKey()
          : instance(nullptr)
      {
      }
      LocatorKey(const LocatorKey &other)
          : instance(other.instance),
            tableName(other.tableName)
      {
      }
      /**
   * Constructor
   * @param instance intance implementation
   * @param table table for which we are locating tablets
   **/
      explicit LocatorKey(std::shared_ptr<cclient::data::Instance> instance, std::string table);
      // instance
      std::shared_ptr<cclient::data::Instance> instance;
      // table name
      std::string tableName;

      LocatorKey &operator=(const LocatorKey &other)
      {
        instance = other.instance;
        tableName = other.tableName;
        return *this;
      }
      bool operator==(const LocatorKey &key)
      {
        return instance->getInstanceId() == key.instance->getInstanceId() && tableName == key.tableName;
      }

      bool operator>(const LocatorKey &key) const
      {
        if (instance == nullptr)
          return false;
        return instance->getInstanceId() >= key.instance->getInstanceId() && tableName > key.tableName;
      }

      bool operator<(const LocatorKey &key) const
      {
        if (instance == nullptr)
          return true;
        return instance->getInstanceId() <= key.instance->getInstanceId() && tableName < key.tableName;
      }
    };

    /**
 * Tablet location mechanism
 * Purpose: To locate tablets given ranges and mutations
 **/
    class TabletLocator
    {
    public:
      /**
   * Constructor
   **/
      TabletLocator();
      virtual ~TabletLocator();

      /**
   * Locate tablets
   * @param creds credentials
   * @param row tablet row that is being located
   * @param skipRow determines if we skip rows based on inclusion/exclusion
   * @param retry retries locations if a failure occurs
   * @returns returns the tablet location or null
   **/
      virtual cclient::data::TabletLocation locateTablet(cclient::data::security::AuthInfo *creds, std::string row, bool skipRow, bool retry) = 0;

      /**
   * returns a list of locations
   * @returns list of tablet locations
   **/
      virtual std::vector<cclient::data::TabletLocation> locations(cclient::data::security::AuthInfo *credentials)
      {
        return std::vector<cclient::data::TabletLocation>();
      }

      /**
   * Bins mutations
   * @param credentials connecting user's credentials
   * @param mutations mutations to bin
   * @param binnedmutations binned mutations according to the server hosting the data
   * @param locations servers hosting the data
   * @param failures failed binned locations
   **/
      virtual void
      binMutations(cclient::data::security::AuthInfo *credentials, std::vector<std::shared_ptr<cclient::data::Mutation>> *mutations,
                   std::map<std::string, std::shared_ptr<cclient::data::TabletServerMutations>> *binnedMutations, std::set<std::string> *locations,
                   std::vector<std::shared_ptr<cclient::data::Mutation>> *failures) = 0;

      /**
   * Bins ranges
   * @param credentials connecting user's credentials
   * @param mutations mutations to bin
   * @param binnedmutations binned mutations according to the server hosting the data
   * @param locations servers hosting the data
   * @param failures failed binned locations
   **/
      virtual std::vector<std::shared_ptr<cclient::data::Range>>
      binRanges(
          cclient::data::security::AuthInfo *credentials,
          std::vector<std::shared_ptr<cclient::data::Range>> *ranges,
          std::set<std::string> *locations,
          std::map<std::string, std::map<std::shared_ptr<cclient::data::KeyExtent>, std::vector<std::shared_ptr<cclient::data::Range>>, pointer_comparator<std::shared_ptr<cclient::data::KeyExtent>>>> *binnedRanges) = 0;

      /**
   * Invalides the cache for the failed key extent
   * @param failedExtent extent that failed
   **/
      virtual void
      invalidateCache(cclient::data::KeyExtent failedExtent) = 0;

      /**
   * Invalides key extent caches
   **/
      virtual void
      invalidateCache() = 0;
      /**
   * Invalides the cache for the failed key extent
   * @param failedExtent extent that failed
   **/
      virtual void
      invalidateCache(std::vector<cclient::data::KeyExtent> keySet) = 0;
    };

  } // namespace impl
} /* namespace cclient */
#endif /* EXTENTLOCATOR_H_ */
