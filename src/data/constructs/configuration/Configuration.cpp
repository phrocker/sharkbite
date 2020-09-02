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
#include "data/constructs/configuration/Configuration.h"

#include <cstdlib>
#include <map>
#include <string>

namespace cclient {
namespace impl {

Configuration::~Configuration() {}

void Configuration::set(const std::string &name, const std::string &value) {
  configurationMap[name] = value;
}

std::string Configuration::get(const std::string &name) const {
  return get(name, "");
}

std::string Configuration::get(const std::string &name,
                               const std::string &def) const {
  auto getter = configurationMap.find(name);
  if (getter == configurationMap.end()) {
    return def;
  } else {
    return getter->second;
  }
}

uint32_t Configuration::getLong(const std::string &name) const {
  return std::atol(get(name, 0).c_str());
}

uint32_t Configuration::getLong(const std::string &name, uint32_t def) const {
  auto getter = configurationMap.find(name);
  if (getter == configurationMap.end()) {
    return def;
  } else
    return atol(getter->second.c_str());
}

} /* namespace impl */
} /* namespace cclient */
