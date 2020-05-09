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
#ifndef INCLUDE_DATA_STREAMING_ACCUMULO_STREAMSEEKABLE_H_
#define INCLUDE_DATA_STREAMING_ACCUMULO_STREAMSEEKABLE_H_

#include <cstdlib>

#include <vector>
#include "../StreamRelocation.h"
#include "../../constructs/Range.h"
#include "data/constructs/security/Authorizations.h"

namespace cclient {
namespace data {
namespace streams {

class StreamSeekable : public StreamRelocation {
 protected:
  Range range;
  std::vector<std::string> columnFamilies;
  bool inclusive;
  cclient::data::security::Authorizations auths;
 public:

  explicit StreamSeekable(Range &range)
      :
      range(std::move(range)),
      inclusive(false) {

  }

  explicit StreamSeekable(Range &range, std::vector<std::string> &columnFamilies, bool inclusive)
      :
      range(std::move(range)),
      columnFamilies(columnFamilies),
      inclusive(inclusive) {

  }

  explicit StreamSeekable(Range &range, std::vector<std::string> &columnFamilies, cclient::data::security::Authorizations &in_auths, bool inclusive)
      :
      range(std::move(range)),
      columnFamilies(columnFamilies),
      inclusive(inclusive),
      auths(std::move(in_auths)) {

  }

  explicit StreamSeekable(Range *rng, std::vector<std::string> &columnFamilies, cclient::data::security::Authorizations *in_auths, bool inclusive)
      :
      range(rng->getStartKey(), rng->getStartKeyInclusive(), rng->getStopKey(), rng->getStartKeyInclusive()),
      columnFamilies(columnFamilies),
      inclusive(inclusive),
      auths(in_auths->getAuthorizations()) {

  }

  virtual cclient::data::security::Authorizations* getAuths() override {
    return &auths;
  }

  virtual Range* getRange() override
  {
    return &range;
  }

  virtual std::vector<std::string>* getColumnFamilies() override
  {
    return &columnFamilies;
  }

  virtual bool isInclusive() override
  {
    return inclusive;
  }
};

}
}
}

#endif /* INCLUDE_DATA_STREAMING_ACCUMULO_STREAMSEEKABLE_H_ */
