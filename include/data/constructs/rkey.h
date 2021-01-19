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

#ifndef R_KEY
#define R_KEY

#include "Key.h"
#include <stdint.h>

#include "../streaming/Streams.h"
#include "data/constructs/AllocatorPool.h"

#include <stdint.h>
#include <cstdio>
#include <vector>
#include <cstring>
#include <stdexcept>

#include "data/constructs/security/Authorizations.h"
#include "data/extern/fastmemcpy/FastMemCpy.h"
#include "data/constructs/security/VisibilityEvaluator.h"
#include "data/constructs/ageoff/AgeOffConditions.h"
#include "data/constructs/predicates/key/KeyPredicates.h"

namespace cclient {
namespace data {

/**
 * Based on the accumulo relative key.
 */
class RelativeKey : public cclient::data::streams::StreamInterface {

 public:
  /**
   * Constructor
   **/
  explicit RelativeKey(ArrayAllocatorPool *allocins);

  /**
   * Constructor.
   * @param previous_key previous key
   * @param my_key current key.
   **/
  explicit RelativeKey(const std::shared_ptr<Key> &previous_key, const std::shared_ptr<Key> &my_key, ArrayAllocatorPool *allocins);

  /**
   * Returns the relative key.
   **/
  std::shared_ptr<StreamInterface>
  getStream();

  /**
   * Sets the base for the relative key
   * @param my_key key to set on the relative key.
   **/
  void
  setBase(const std::shared_ptr<Key> &my_key);

  /**
   * create and fulfill the relative key object.
   **/
  virtual uint64_t read(cclient::data::streams::InputStream *in);

  uint64_t readFiltered(cclient::data::streams::InputStream *in);

  /**
   * Sets the previous key for this relative key.
   * @param previous_key prev key.
   **/
  void
  setPrevious(const std::shared_ptr<Key> &previous_key);

  /**
   * write function for relative key
   * @param outStream output stream.
   **/
  uint64_t
  write(cclient::data::streams::OutputStream *outStream);

  bool operator <(const RelativeKey &rhs) const;

  bool operator <(const RelativeKey *rhs) const;

  ~RelativeKey();

  // flags taken from java relative key
  static const uint8_t ROW_SAME = 0x01;
  static const uint8_t CF_SAME = 0x02;
  static const uint8_t CQ_SAME = 0x04;
  static const uint8_t CV_SAME = 0x08;
  static const uint8_t TS_SAME = 0x10;
  static const uint8_t DELETED = 0x20;

  static const uint8_t ROW_PREFIX = 0x01;
  static const uint8_t CF_PREFIX = 0x02;
  static const uint8_t CQ_PREFIX = 0x04;
  static const uint8_t CV_PREFIX = 0x08;
  static const uint8_t TS_DIFF = 0x10;

  static const uint8_t PREFIX_COMPRESSION_ENABLED = 128;

  const std::shared_ptr<Key> getKey() const {
    return key;
  }

  bool isFiltered() const {
    return filtered;
  }

  void filterVisibility(const cclient::data::security::Authorizations &visibility, const std::shared_ptr<cclient::data::security::VisibilityEvaluator> &eval = nullptr);

  void setAgeOffEvaluator(const std::shared_ptr<cclient::data::AgeOffEvaluator> &conditions);

  void setKeyPredicate(const std::shared_ptr<cclient::data::KeyPredicate> &conditions);

  void setFiltered();

 protected:

  int INLINE readPrefix(cclient::data::streams::InputStream *stream, std::pair<char*, size_t> *row, std::pair<char*, size_t> *prevRow, const size_t &prevsize, bool &disown);

  bool INLINE readRow(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                      const std::shared_ptr<Key> &newkey);

  bool INLINE readCf(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                     const std::shared_ptr<Key> &newkey);

  bool INLINE readCq(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                     const std::shared_ptr<Key> &newkey);

  bool INLINE readCv(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                     const std::shared_ptr<Key> &newkey);

  std::shared_ptr<cclient::data::Key> INLINE readRowFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                      std::shared_ptr<Key> &newkey);

  bool INLINE readCfFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                     const std::shared_ptr<Key> &newkey);

  bool INLINE readCqFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                     const std::shared_ptr<Key> &newkey);

  bool INLINE readCvFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                     const std::shared_ptr<Key> &newkey);

  int INLINE read(cclient::data::streams::InputStream *stream, std::pair<char*, size_t> *row);

  int INLINE read(cclient::data::streams::InputStream *stream, std::pair<char*, size_t> *input, uint32_t len);

  int
  INLINE commonPrefix(std::pair<char*, size_t> prev, std::pair<char*, size_t> curr);

  void INLINE writePrefix(cclient::data::streams::OutputStream *outStream, std::pair<char*, size_t> *var, int commonPrefixLength) {
    // should be writevint, below
    outStream->writeVLong(commonPrefixLength);
    uint32_t remainder = var->second - commonPrefixLength;
    outStream->writeVLong(remainder);
    outStream->writeBytes(var->first + commonPrefixLength, remainder);

  }

  std::shared_ptr<Key> key;

  char fieldsSame;

  std::shared_ptr<Key> prevKey;

  void
  setKey(const std::shared_ptr<Key> &keyToCopy, const std::shared_ptr<Key> &keyToCopyTo);

  bool
  INLINE isSame(std::pair<char*, size_t> a, std::pair<char*, size_t> b);

  bool filtered;
  bool prevFiltered;
  bool likelyFiltered;

  cclient::data::security::Authorizations columnVisibility;
  std::shared_ptr<cclient::data::security::VisibilityEvaluator> evaluator;

  std::shared_ptr<cclient::data::AgeOffEvaluator> ageOffEvaluator;

  std::shared_ptr<cclient::data::KeyPredicate> keyPredicate;


  int32_t rowCommonPrefixLen;
  int32_t cfCommonPrefixLen;
  int32_t cqCommonPrefixLen;
  int32_t cvCommonPrefixLen;
  uint8_t fieldsPrefixed;
  long tsDiff;

  std::shared_ptr<Text> row_ref;

  bool row_ref_dirty;
  std::shared_ptr<Text> cf_ref;
  bool cf_ref_dirty;
  std::shared_ptr<Text> cq_ref;
  bool cq_ref_dirty;
  std::shared_ptr<Text> cv_ref;
  bool cv_ref_dirty;

  ArrayAllocatorPool *allocatorInstance;
};
}
}

#endif

