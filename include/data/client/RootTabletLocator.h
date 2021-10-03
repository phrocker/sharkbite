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

#ifndef DATA_CLIENT_IMPL_METADATA_ROOTTABLETLOCATOR_H_
#define DATA_CLIENT_IMPL_METADATA_ROOTTABLETLOCATOR_H_

#include <set>
#include <thread>
#include <vector>

#include "../constructs/StructureDefinitions.h"
#include "../constructs/client/Instance.h"
#include "../constructs/client/zookeeperinstance.h"
#include "../constructs/column.h"
#include "../constructs/security/AuthInfo.h"
#include "ExtentLocator.h"
#include "TabletLocationObtainer.h"
namespace cclient {
namespace impl {

static std::shared_ptr<cclient::data::KeyExtent> ROOT_EXTENT =
    std::make_shared<cclient::data::KeyExtent>("+r", "", "");

class RootTabletLocator : public TabletLocator {
 public:
  explicit RootTabletLocator(std::shared_ptr<cclient::data::Instance> instance);
  ~RootTabletLocator();

  cclient::data::TabletLocation locateTablet(
      cclient::data::security::AuthInfo *creds, const std::string  &row, bool skipRow,
      bool retry) override{
    cclient::data::TabletLocation *location = getRootTabletLocation();

    while (retry && location == NULL) {
      std::this_thread::yield();

      location = getRootTabletLocation();
    }
    if (location) {
      cclient::data::TabletLocation te(location);
      delete location;
      return te;
    } else {
      throw std::runtime_error("Could not locate root tablet");
    }
  }

  void binMutations(
      cclient::data::security::AuthInfo *credentials,
      std::vector<std::shared_ptr<cclient::data::Mutation>> *mutations,
      std::map<std::string,
               std::shared_ptr<cclient::data::TabletServerMutations>>
          *binnedMutations,
      std::set<std::string> *locations,
      std::vector<std::shared_ptr<cclient::data::Mutation>> *failures) override {}

  virtual std::vector<std::shared_ptr<cclient::data::Range>> binRanges(
      cclient::data::security::AuthInfo *credentials,
      std::vector<std::shared_ptr<cclient::data::Range>> *ranges,
      std::set<std::string> *locations,
      std::map<std::string,
               std::map<std::shared_ptr<cclient::data::KeyExtent>,
                        std::vector<std::shared_ptr<cclient::data::Range>>,
                        pointer_comparator<
                            std::shared_ptr<cclient::data::KeyExtent>>>>
          *binnedRanges) override{
    return std::vector<std::shared_ptr<cclient::data::Range>>();
  }

  void invalidateCache(cclient::data::KeyExtent failedExtent) override{}

  void invalidateCache() override{}

  void invalidateCache(std::vector<cclient::data::KeyExtent> keySet) override {}

 protected:
  cclient::data::TabletLocation *getRootTabletLocation();
  std::shared_ptr<cclient::data::zookeeper::ZookeeperInstance> myInstance;
};
}  // namespace impl
}  // namespace cclient
#endif /* DATA_CLIENT_IMPL_METADATA_ROOTTABLETLOCATOR_H_ */
