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

#include "data/constructs/rfile/meta/LocalityGroupIterator.h"

#include <algorithm>

#include "data/streaming/StreamRelocation.h"
#include "data/streaming/accumulo/StreamSeekable.h"

namespace cclient {
namespace data {

std::shared_ptr<LocalityGroupSeekCache> LocalityGroupIterator::relocate(
    const std::shared_ptr<HeapIterator> &hiter, LocalityGroupContext *lgContext,
    const std::shared_ptr<cclient::data::streams::StreamRelocation> &location,
    const std::shared_ptr<LocalityGroupSeekCache> &lgSeekCache) {
  // determine if the arguments have changed since the last time
  bool sameArgs = false;

  std::set<std::string> cfSet;
  for (auto cf : *location->getColumnFamilies()) {
    cfSet.insert(cf);
  }
  if (!lgSeekCache->lastUsed.empty() &&
      location->isInclusive() == lgSeekCache->lastInclusive) {
    sameArgs = lgSeekCache->lastColumnFamilies == (cfSet);
  }

  // if the column families and inclusiveness have not changed, then we can
  // simply re-seek the locality groups we discovered last round and rebuild the
  // heap.
  if (sameArgs) {
    hiter->clear();
    for (auto &lgr : lgSeekCache->lastUsed) {
      std::vector<std::string> empty_cf;
      auto new_relocation =
          std::make_shared<cclient::data::streams::StreamSeekable>(
              location->getRange(), empty_cf, location->getAuths(), false);
      lgr->getIterator()->relocate(new_relocation);
      hiter->addSource(lgr->getIterator());
    }
  } else {  // otherwise capture the parameters, and use the static seek method
            // to locate the locality groups to use.
    lgSeekCache->lastColumnFamilies = cfSet;
    lgSeekCache->lastInclusive = location->isInclusive();
    lgSeekCache->lastUsed = _relocate(hiter, lgContext, location);
  }

  return lgSeekCache;
}

std::vector<std::shared_ptr<LocalityGroup>> LocalityGroupIterator::_relocate(
    const std::shared_ptr<HeapIterator> &hiter, LocalityGroupContext *lgContext,
    const std::shared_ptr<cclient::data::streams::StreamRelocation> &location) {
  hiter->clear();

  std::set<std::string> cfSet;
  for (auto cf : *location->getColumnFamilies()) {
    cfSet.insert(cf);
  }

  bool inclusive = location->isInclusive();

  // determine the set of groups to use
  std::vector<std::shared_ptr<LocalityGroup>> groups;

  // if no column families specified, then include all groups unless !inclusive
  if (cfSet.empty()) {
    if (!inclusive) {
      groups = lgContext->groups;
    }
  } else {
    // do not know what column families are in the default locality group,
    // only know what column families are not in it
    if (lgContext->defaultGroup != nullptr) {
      if (inclusive) {
        if (!std::all_of(cfSet.begin(), cfSet.end(),
                         [lgContext](const std::string &str) {
                           return lgContext->groupByCf.find(str) !=
                                  lgContext->groupByCf.end();
                         })) {
          // lgContext->groupByCf.keySet().containsAll(cfSet)
          // default LG may contain wanted and unwanted column families
          groups.push_back(lgContext->defaultGroup);
        }  // else - everything wanted is in other locality groups, so nothing
           // to do
      } else {
        // must include the default group as it may include cfs not in our cfSet
        groups.push_back(lgContext->defaultGroup);
      }
    }

    /*
     * Need to consider the following cases for inclusive and exclusive
     * (lgcf:locality group column family set, cf:column family set) lgcf and cf
     * are disjoint lgcf and cf are the same cf contains lgcf lgcf contains cf
     * lgccf and cf intersect but neither is a subset of the other
     */
    if (!inclusive) {
      for (const auto &entry : lgContext->groupByCf) {
        if (cfSet.find(entry.first) == cfSet.end()) {
          groups.push_back(entry.second);
        }
      }
    } else if (lgContext->groupByCf.size() <= cfSet.size()) {
      for (const auto &entry : lgContext->groupByCf) {
        if (cfSet.find(entry.first) != cfSet.end()) {
          groups.push_back(entry.second);
        }
      }
    } else {
      for (std::string cf : cfSet) {
        auto fd = lgContext->groupByCf.find(cf);
        if (fd != lgContext->groupByCf.end()) {
          groups.push_back(fd->second);
        }
      }
    }
  }
  std::vector<std::string> empty_cf;
  auto new_relocation =
      std::make_shared<cclient::data::streams::StreamSeekable>(
          location->getRange(), empty_cf, location->getAuths(), false);
  for (const auto &lgr : groups) {
    lgr->getIterator()->relocate(new_relocation);
    hiter->addSource(lgr->getIterator());
  }

  return groups;
}

}  // namespace data
}  // namespace cclient