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
#ifndef SRC_DATA_CONSTRUCTS_CLIENT_INSTANCECACHE_H_
#define SRC_DATA_CONSTRUCTS_CLIENT_INSTANCECACHE_H_

#include <string>



namespace cclient {
namespace data {

class InstanceCache {
public:
    InstanceCache() {
    }
    virtual ~InstanceCache() {
    }

    virtual uint8_t *getData(std::string path) = 0;
    virtual std::vector<std::string> getChildren(const std::string path, bool force=false) = 0;
};

} /* namespace impl */
} /* namespace cclient */

#endif /* SRC_DATA_CONSTRUCTS_CLIENT_INSTANCECACHE_H_ */
