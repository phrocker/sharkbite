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
#ifndef INCLUDE_INTERCONNECT_STREAMING_STREAMRELOCATION_H_
#define INCLUDE_INTERCONNECT_STREAMING_STREAMRELOCATION_H_

#include "data/constructs/Range.h"
#include "data/constructs/security/Authorizations.h"

namespace cclient {
namespace data {
namespace streams {

class StreamRelocation {
 public:
  StreamRelocation() {}

  virtual ~StreamRelocation() {}

  virtual cclient::data::security::Authorizations* getAuths() = 0;

  virtual cclient::data::Range* getRange() = 0;

  virtual std::vector<std::string>* getColumnFamilies() = 0;

  virtual uint32_t getDesiredThreads() const = 0;

  virtual uint32_t getKeysCachedPerThread() const = 0;

  virtual bool isInclusive() = 0;
};

}  // namespace streams
}  // namespace data
}  // namespace cclient

#endif /* INCLUDE_INTERCONNECT_STREAMING_STREAMRELOCATION_H_ */
