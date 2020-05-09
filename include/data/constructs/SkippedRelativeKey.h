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
#ifndef INCLUDE_DATA_CONSTRUCTS_SKIPPEDRELATIVEKEY_H_
#define INCLUDE_DATA_CONSTRUCTS_SKIPPEDRELATIVEKEY_H_

#include "Key.h"
#include "rkey.h"
#include <stdint.h>

#include "../streaming/Streams.h"
#include "data/constructs/security/Authorizations.h"
#include "data/constructs/security/VisibilityEvaluator.h"
#include "logging/Logger.h"
#include "logging/LoggerConfiguration.h"
#include <stdint.h>
#include <cstdio>
#include <stdexcept>
#include <vector>

namespace cclient {
namespace data {

struct TextBuffer{
  char *buffer;
  size_t size;
  size_t max;

  bool operator>(const TextBuffer &o) const{
    if (strncmp(buffer,o.buffer,size > o.size ? o.size : size)>0)
    return true;
    else return false;
  }

   bool operator>=(const TextBuffer &o) const{
    if (strncmp(buffer,o.buffer,size > o.size ? o.size : size)>=0)
    return true;
    else return false;
  }

  TextBuffer operator=(const std::pair<char*,size_t> other){
    size = other.second;
    max = size;
    if (max <= 32){
      max = 32;
    }
    else if (max <= 64){
      max = 64;
    }else if (max <= 128){
      max = 128;
    }
    buffer = new char[ max];
    memcpy_fast(buffer,other.first,size);

    return *this;
  }
};
/**
 * Based on the accumulo relative key.
 */
class SkippedRelativeKey : public cclient::data::streams::StreamInterface {
 private:
 static const uint8_t PREFIX_COMPRESSION_ENABLED = 128;
 private:
  std::shared_ptr<logging::Logger> logger;

 protected:
  std::shared_ptr<Key> prevKey;
  std::shared_ptr<RelativeKey> rkey;
  //std::string colvis;
  int skipped;
  ArrayAllocatorPool *allocatorInstance;
  cclient::data::security::Authorizations auths;

  bool fastSkip(cclient::data::streams::InputStream *stream, const std::shared_ptr<Key> &seekKey, std::vector<char> *valCopy, std::shared_ptr<Key> prevKey, std::shared_ptr<Key> currKey,
                size_t entriesRemaining);

  bool readPrefix(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame,int fieldsPrefixed, TextBuffer *field, TextBuffer *prevField,
                  TextBuffer *stopField  );

  void readPrefix(cclient::data::streams::InputStream *stream, TextBuffer *row, TextBuffer *prevRow);

  void readValue(cclient::data::streams::InputStream *stream, std::vector<char> *val);

  void read(cclient::data::streams::InputStream *stream, TextBuffer *row);

  void read(cclient::data::streams::InputStream *stream, TextBuffer *row, uint32_t len);

 public:
  explicit SkippedRelativeKey(RelativeKey *rkey, int skipped, std::shared_ptr<Key> prevKey, ArrayAllocatorPool *allocator)
      :
      logger(logging::LoggerFactory<SkippedRelativeKey>::getLogger()),
      rkey(rkey),
      allocatorInstance(allocator),
      skipped(skipped),
      prevKey(prevKey) {

  }

  SkippedRelativeKey(ArrayAllocatorPool *allocator)
      :
      SkippedRelativeKey(NULL, 0, NULL, allocator) {
  }

  bool skip(cclient::data::streams::InputStream *stream, std::shared_ptr<Key> seekKey, std::vector<char> *valCopy, std::shared_ptr<Key> prevKey, std::shared_ptr<Key> currKey,
            size_t entriesRemaining) {
    return fastSkip(stream, seekKey, valCopy, prevKey, currKey, entriesRemaining);
  }

  void filterVisibility(const cclient::data::security::Authorizations &auths) {
    this->auths = auths;
  }

  virtual ~SkippedRelativeKey() {
  }

  std::shared_ptr<RelativeKey> getRelativeKey() {
    return rkey;
  }
  std::shared_ptr<Key> getPrevKey() {
    return prevKey;
  }
  int getSkipped() {
    return skipped;
  }

}
;

}
}

#endif /* INCLUDE_DATA_CONSTRUCTS_SKIPPEDRELATIVEKEY_H_ */
