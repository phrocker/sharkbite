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

#include "data/client/TabletServerLocator.h"

#include "data/client/TabletLocation.h"
#include "data/client/TabletLocationObtainer.h"

namespace cclient {
namespace impl {

TabletServerLocator::TabletServerLocator(
    std::string tableId, TabletLocator *parent, TabletLocationObtainer *lc,
    std::shared_ptr<cclient::data::Instance> inst)
    : tableId(tableId),
      parent(parent),
      locator(lc),
      instance(inst),
      lastTabletRow(tableId),
      logger(logging::LoggerFactory<TabletServerLocator>::getLogger()) {
  lastTabletRow.append("<");
}

TabletServerLocator::~TabletServerLocator() { invalidateCache(); }

}  // namespace impl
} /* namespace cclient */
