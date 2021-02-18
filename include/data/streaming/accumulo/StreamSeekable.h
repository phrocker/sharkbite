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

#include "../../constructs/Range.h"
#include "../StreamRelocation.h"
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
  uint32_t threads;
  uint32_t keysPerThread;

 public:
  explicit StreamSeekable(Range &range, uint32_t threads = 1,
                          uint32_t keysPerThread = 1000)
      : range(std::move(range)), inclusive(false), threads(threads) {}

  explicit StreamSeekable(Range &range,
                          std::vector<std::string> &columnFamilies,
                          bool inclusive, uint32_t threads = 1,
                          uint32_t keysPerThread = 1000)
      : range(std::move(range)),
        columnFamilies(columnFamilies),
        inclusive(inclusive),
        threads(threads),
        keysPerThread(keysPerThread) {}

  explicit StreamSeekable(Range &range,
                          std::vector<std::string> &columnFamilies,
                          cclient::data::security::Authorizations &in_auths,
                          bool inclusive, uint32_t threads = 1,
                          uint32_t keysPerThread = 1000)
      : range(std::move(range)),
        columnFamilies(columnFamilies),
        inclusive(inclusive),
        auths(std::move(in_auths)),
        threads(threads),
        keysPerThread(keysPerThread) {}

  explicit StreamSeekable(Range *rng, std::vector<std::string> &columnFamilies,
                          cclient::data::security::Authorizations *in_auths,
                          bool inclusive, uint32_t threads = 1,
                          uint32_t keysPerThread = 1000)
      : range(rng->getStartKey(), rng->getStartKeyInclusive(),
              rng->getStopKey(), rng->getStartKeyInclusive()),
        columnFamilies(columnFamilies),
        inclusive(inclusive),
        auths(in_auths->getAuthorizations()),
        threads(threads),
        keysPerThread(keysPerThread) {}

  virtual cclient::data::security::Authorizations *getAuths() override {
    return &auths;
  }

  virtual uint32_t getDesiredThreads() const override { return threads; }

  void setThreads(uint32_t threads) { this->threads = threads; }

  virtual uint32_t getKeysCachedPerThread() const override {
    return keysPerThread;
  }

  virtual Range *getRange() override { return &range; }

  virtual std::vector<std::string> *getColumnFamilies() override {
    return &columnFamilies;
  }

  virtual bool isInclusive() override { return inclusive; }
};

class DelegatedStreamSeekable : public StreamRelocation {
 protected:
  std::shared_ptr<Range> range;
  StreamRelocation *seekable;

 public:
  explicit DelegatedStreamSeekable(std::shared_ptr<Range> &rng,
                                   StreamRelocation *ssk)
      : range(rng), seekable(ssk) {}

  virtual cclient::data::security::Authorizations *getAuths() override {
    return seekable->getAuths();
  }

  virtual uint32_t getDesiredThreads() const override {
    return seekable->getDesiredThreads();
  }

  virtual uint32_t getKeysCachedPerThread() const override {
    return seekable->getKeysCachedPerThread();
  }

  virtual Range *getRange() override { return range.get(); }

  virtual std::vector<std::string> *getColumnFamilies() override {
    return seekable->getColumnFamilies();
  }

  virtual bool isInclusive() override { return seekable->isInclusive(); }
};

}  // namespace streams
}  // namespace data
}  // namespace cclient

#endif /* INCLUDE_DATA_STREAMING_ACCUMULO_STREAMSEEKABLE_H_ */
