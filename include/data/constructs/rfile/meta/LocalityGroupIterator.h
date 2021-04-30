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
#pragma once

#include <map>
#include <set>
#include <vector>
#include <memory>
#include "data/iterators/HeapIterator.h"
#include "data/constructs/Range.h"
#include "data/streaming/accumulo/KeyValueIterator.h"


namespace cclient {
namespace data {

class LocalityGroup{
public:
    bool defaultGroup = {false};
    std::map<std::string,long> columnFamilies;

    virtual std::shared_ptr<cclient::data::streams::KeyValueIterator> getIterator() = 0;    
    

    auto isDefaultLocalityGroup() const{
      return defaultGroup;
    }

    
};

class LocalityGroupSeekCache{
public:

    LocalityGroupSeekCache(){}

    auto getLastColumnFamilies() const {
      return lastColumnFamilies;
    }

    auto isLastInclusive() const{
      return lastInclusive;
    }

    auto getLastUsed() const{
      return lastUsed;
    }

    auto getNumLGSeeked() const {
      return lastUsed.size();
    }

   
    std::set<std::string> lastColumnFamilies;
    bool lastInclusive;
    std::vector<std::shared_ptr<LocalityGroup>> lastUsed;
    
};


class LocalityGroupContext {
public:

std::vector<std::shared_ptr<LocalityGroup>> groups;
std::shared_ptr<LocalityGroup> defaultGroup;
std::map<std::string,std::shared_ptr<LocalityGroup>> groupByCf;


explicit LocalityGroupContext(std::vector<std::shared_ptr<LocalityGroup>> groups) : groups(groups){
    std::shared_ptr<LocalityGroup> foundDefault = nullptr;
    for (const auto &group : groups) {
    if (group->isDefaultLocalityGroup() && group->columnFamilies.empty()) {
        if (foundDefault != nullptr) {
          throw std::runtime_error("Found multiple default locality groups");
        }
        foundDefault = group;
    } else {
        for (const auto entry: group->columnFamilies) {
        if (entry.second > 0) {
            if (groupByCf.find(entry.first) != groupByCf.end()) {
            throw std::runtime_error("Found the same cf in multiple locality groups");
            }
            groupByCf.insert(std::pair<std::string,std::shared_ptr<LocalityGroup>>(entry.first, group));
        }
        }
    }
    }
    defaultGroup = foundDefault;
}
};

class LocalityGroupIterator : public cclient::data::HeapIterator{
public:

    static std::shared_ptr<LocalityGroupSeekCache > relocate(const std::shared_ptr<HeapIterator> &hiter, LocalityGroupContext *lgContext, cclient::data::streams::StreamRelocation *location, const std::shared_ptr<LocalityGroupSeekCache > &lgSeekCache );

protected:

    static std::vector<std::shared_ptr<LocalityGroup>> _relocate(const std::shared_ptr<HeapIterator> &hiter, LocalityGroupContext *lgContext, cclient::data::streams::StreamRelocation *location);

};

}
}