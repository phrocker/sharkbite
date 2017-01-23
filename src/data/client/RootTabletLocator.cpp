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

#include <sstream>

#include "../../../include/data/client/RootTabletLocator.h"
#include "../../../include/data/client/../constructs/client/Instance.h"
#include "../../../include/data/client/ExtentLocator.h"
namespace cclient{
  namespace impl{


RootTabletLocator::RootTabletLocator (cclient::data::Instance *instance) :
    myInstance (dynamic_cast<cclient::data::zookeeper::ZookeeperInstance*> (instance))
{

}

RootTabletLocator::~RootTabletLocator ()
{

}

cclient::data::TabletLocation *
RootTabletLocator::getRootTabletLocation ()
{

    cclient::data::zookeeper::ZooCache *cache = myInstance->getZooCache ();

    std::stringstream zRootLocation;
    zRootLocation << myInstance->getRoot () << ZROOT_TABLET_LOCATION;

    uint8_t *rootLoc = cache->getData (zRootLocation.str ());

    if (IsEmpty ((char*) rootLoc))
    {
        return 0;
    }

    std::vector<std::string> tokens = split (std::string ((char*) rootLoc), '|');

    return new cclient::data::TabletLocation (new cclient::data::KeyExtent( ROOT_EXTENT), tokens.at (0), tokens.at (1));

}

  }
}