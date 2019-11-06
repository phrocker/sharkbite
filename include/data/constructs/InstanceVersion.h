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

#include <string>
#include <map>
#include <mutex>

namespace cclient {
namespace data {

class InstanceVersion {

 public:

  static int getVersion(const std::string &instance) {
    const InstanceVersion *mapper = getInstance();
    return mapper->getHostVersion(instance);
  }

  static void setVersion(const std::string &instance, int version) {
    InstanceVersion *mapper = getInstance();
    mapper->setHostVersion(instance, version);
  }

 private:

  static InstanceVersion *getInstance() {
    static InstanceVersion vers;
    return &vers;
  }

  InstanceVersion() {
  }

  void setHostVersion(const std::string &instance, int version) {
    std::lock_guard<std::mutex> lock(mtx);
    version_map_.insert(std::make_pair(instance, version));
  }

  int getHostVersion(const std::string &instance) const {
    std::lock_guard<std::mutex> lock(mtx);
    auto vmap = version_map_.find(instance);
    if (vmap != std::end(version_map_)) {
      return vmap->second;
    } else {
      return 0;
    }
  }

  mutable std::mutex mtx;
  std::map<std::string, int> version_map_;
};

} /* namespace data */
} /* namespace cclient */
