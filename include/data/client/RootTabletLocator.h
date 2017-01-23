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
#include <vector>
#include <thread>

#include "ExtentLocator.h"

#include "../constructs/column.h"
#include "../constructs/StructureDefinitions.h"
#include "../constructs/client/Instance.h"
#include "../constructs/client/zookeeperinstance.h"
#include "TabletLocationObtainer.h"
#include "../constructs/security/AuthInfo.h"
namespace cclient {
namespace impl {


static cclient::data::KeyExtent *ROOT_EXTENT = new cclient::data::KeyExtent("+r", "", "");

class RootTabletLocator: public TabletLocator {
public:
    explicit RootTabletLocator(cclient::data::Instance *instance);
    ~RootTabletLocator();

    cclient::data::TabletLocation *locateTablet(cclient::data::security::AuthInfo *creds, std::string row, bool skipRow, bool retry) {
        cclient::data::TabletLocation *location = getRootTabletLocation();

        while (retry && location == NULL) {
            std::this_thread::yield();

            location = getRootTabletLocation();

        }

        return location;

    }

    void binMutations(cclient::data::security::AuthInfo *credentials, std::vector<cclient::data::Mutation*> *mutations,
                      std::map<std::string, cclient::data::TabletServerMutations*> *binnedMutations,
                      std::set<std::string> *locations, std::vector<cclient::data::Mutation*> *failures) {
    }

    virtual std::vector<cclient::data::Range*> binRanges(cclient::data::security::AuthInfo *credentials,std::vector<cclient::data::Range*> *ranges,
                                     std::set<std::string> *locations,
                                     std::map<std::string, std::map<cclient::data::KeyExtent*, std::vector<cclient::data::Range*>,pointer_comparator<cclient::data::KeyExtent*> > > *binnedRanges) {
        return std::vector<cclient::data::Range*>();
    }

    void invalidateCache(cclient::data::KeyExtent failedExtent) {
    }

    void invalidateCache() {
    }

    void invalidateCache(std::vector<cclient::data::KeyExtent> keySet) {
    }



protected:

    cclient::data::TabletLocation *getRootTabletLocation();
    cclient::data::zookeeper::ZookeeperInstance *myInstance;
};
}
}
#endif /* DATA_CLIENT_IMPL_METADATA_ROOTTABLETLOCATOR_H_ */
