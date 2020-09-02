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

#ifndef SRC_DATA_CLIENT_LOCATORCACHE_H_
#define SRC_DATA_CLIENT_LOCATORCACHE_H_

#include <map>
#include <mutex>

#include "ExtentLocator.h"

namespace cclient {
namespace impl {

/**
 * Tablet Locator cache
 **/
class LocatorCache {
 public:
  /**
   * Constructor that creates the mutex
   **/
  LocatorCache();
  /**
   * Destructor
   **/
  virtual ~LocatorCache();
  /**
   * Puts a tablet locator into the cache
   * @param key cached key
   * @param locator locator to cache
   **/
  void put(LocatorKey key, TabletLocator *locator) {
    std::lock_guard<std::recursive_mutex> lock(locatorMutex);
    locatorCache->insert(std::make_pair(key, locator));
  }

  /**
   * Obtains a tablet locator or NULL if one does not exist
   * @param key locator key
   **/
  TabletLocator *getLocator(LocatorKey key);

 protected:
  // locator map
  std::map<LocatorKey, TabletLocator *> *locatorCache;
  // locator mutex
  std::recursive_mutex locatorMutex;
};

extern LocatorCache cachedLocators;

} /* namespace impl */
} /* namespace cclient */

#endif /* SRC_DATA_CLIENT_LOCATORCACHE_H_ */
