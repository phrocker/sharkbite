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
#include <cstring>
#include <algorithm>
#include "data/constructs/compressor/algorithm.h"
#include "data/constructs/rkey.h"

namespace cclient {
namespace data {

RelativeKey::RelativeKey(ArrayAllocatorPool *alloc)
    :
    fieldsSame(0),
    fieldsPrefixed(0),
    key(NULL),
    filtered(false),
    prevFiltered(false),
    prevKey(NULL),
    row_ref(std::make_shared<Text>(alloc)),
    row_ref_dirty(false),
    cf_ref(std::make_shared<Text>(alloc)),
    cf_ref_dirty(false),
    cq_ref(std::make_shared<Text>(alloc)),
    cq_ref_dirty(false),
    cv_ref(std::make_shared<Text>(alloc)),
    cv_ref_dirty(false),
    likelyFiltered(false),
    tsDiff(0),
    allocatorInstance(alloc) {

}

uint64_t RelativeKey::read(streams::InputStream *stream) {
  filtered = false;
  fieldsSame = stream->readByte();

  if (SH_LIKELY((fieldsSame & PREFIX_COMPRESSION_ENABLED) == PREFIX_COMPRESSION_ENABLED)) {
    fieldsPrefixed = stream->readByte();
  } else {
    fieldsPrefixed = 0;
  }

  uint64_t timestamp = 0;
  uint64_t prevTimestamp = prevKey->getTimeStamp();

  int rowCmp = -1, cfCmp = -1, cqCmp = -1, cvCmp = -1;

  key = allocatorInstance->newKey();

  readRow(stream, &rowCmp, RelativeKey::ROW_SAME, RelativeKey::ROW_PREFIX, fieldsSame, fieldsPrefixed, row_ref, key);

  readCf(stream, &cfCmp, RelativeKey::CF_SAME, RelativeKey::CF_PREFIX, fieldsSame, fieldsPrefixed, cf_ref, key);

  readCq(stream, &cqCmp, RelativeKey::CQ_SAME, RelativeKey::CQ_PREFIX, fieldsSame, fieldsPrefixed, cq_ref, key);

  readCv(stream, &cvCmp, RelativeKey::CV_SAME, RelativeKey::CV_PREFIX, fieldsSame, fieldsPrefixed, cv_ref, key);

  if ((fieldsSame & RelativeKey::TS_SAME) != RelativeKey::TS_SAME) {

    if ((fieldsPrefixed & RelativeKey::TS_DIFF) == RelativeKey::TS_DIFF) {
      timestamp = stream->readEncodedVLong() + prevTimestamp;
    } else {
      timestamp = stream->readEncodedVLong();
    }
  } else {
    timestamp = prevTimestamp;
  }

  if (key) {
    key->setTimeStamp(timestamp);
    prevKey = key;
  }

  // if we've been filtered no point in returning.
  if (filtered) {
    prevFiltered = true;
  }

  return stream->getPos();

}

uint64_t RelativeKey::readFiltered(streams::InputStream *stream) {
  filtered = false;
  fieldsSame = stream->readByte();

  if (SH_LIKELY((fieldsSame & PREFIX_COMPRESSION_ENABLED) == PREFIX_COMPRESSION_ENABLED)) {
    fieldsPrefixed = stream->readByte();
  } else {
    fieldsPrefixed = 0;
  }

  uint64_t timestamp = 0;
  uint64_t prevTimestamp = prevKey->getTimeStamp();

  int rowCmp = -1, cfCmp = -1, cqCmp = -1, cvCmp = -1;

  /**
   * If the previous key was filtered on visibility there is no need to deserialize this key.
   */
  likelyFiltered = ((fieldsSame & RelativeKey::CV_SAME) == RelativeKey::CV_SAME) && prevFiltered;

  if (likelyFiltered) {
    key = nullptr;
  } else {
    key = allocatorInstance->newKey();
  }
  readRowFiltered(stream, &rowCmp, RelativeKey::ROW_SAME, RelativeKey::ROW_PREFIX, fieldsSame, fieldsPrefixed, row_ref, key);

  readCfFiltered(stream, &cfCmp, RelativeKey::CF_SAME, RelativeKey::CF_PREFIX, fieldsSame, fieldsPrefixed, cf_ref, key);

  readCqFiltered(stream, &cqCmp, RelativeKey::CQ_SAME, RelativeKey::CQ_PREFIX, fieldsSame, fieldsPrefixed, cq_ref, key);

  readCvFiltered(stream, &cvCmp, RelativeKey::CV_SAME, RelativeKey::CV_PREFIX, fieldsSame, fieldsPrefixed, cv_ref, key);

  if ((fieldsSame & RelativeKey::TS_SAME) != RelativeKey::TS_SAME) {

    if ((fieldsPrefixed & RelativeKey::TS_DIFF) == RelativeKey::TS_DIFF) {
      timestamp = stream->readEncodedVLong() + prevTimestamp;
    } else {
      timestamp = stream->readEncodedVLong();
    }
  } else {
    timestamp = prevTimestamp;
  }

  if (key) {
    key->setTimeStamp(timestamp);
    prevKey = key;
  }

  // if we've been filtered no point in returning.
  if (filtered) {
    prevFiltered = true;
  }

  return stream->getPos();

}

void RelativeKey::filterVisibility(const cclient::data::security::Authorizations &visibility, const std::shared_ptr<cclient::data::security::VisibilityEvaluator> &eval) {
  columnVisibility = visibility;
  evaluator = eval == nullptr ? std::make_shared<cclient::data::security::VisibilityEvaluator>(columnVisibility) : eval;
}

void RelativeKey::setFiltered() {
  prevFiltered = true;
}

int RelativeKey::commonPrefix(std::pair<char*, size_t> prev, std::pair<char*, size_t> curr) {
  
  if (prev.second==curr.second && !AVX_memcmp(prev.first,curr.first,prev.second,0))
    return -1;  // infinite... exact match

  int prevLen = prev.second;
  int curLen = curr.second;
  int maxChecks = std::min(prevLen, curLen);
  int common = 0;
  while (common < maxChecks) {
    int a = prev.first[common] & 0xff;
    int b = curr.first[common] & 0xff;
    if (a != b)
      return common;
    common++;
  }
  // no differences found
  // either exact or matches the part checked, so if they are the same length, they are an exact match,
  // and if not, then they have a common prefix over all the checks we've done
  return prevLen == curLen ? -1 : maxChecks;
}

RelativeKey::RelativeKey(const std::shared_ptr<Key> &previous_key, const std::shared_ptr<Key> &my_key, ArrayAllocatorPool *alloc)
    :
    RelativeKey(alloc) {
  if (my_key == NULL)
    throw std::runtime_error("Key must not be null");

  //key = std::make_shared<Key>(allocatorInstance);
  key = my_key;
  prevKey = NULL;
  fieldsSame = 0;
  fieldsPrefixed = 0;
  //setKey(my_key, key);

  if (previous_key != NULL) {
    prevKey = previous_key; // std::make_shared<Key>(allocatorInstance);
    //setKey(previous_key, prevKey);

    rowCommonPrefixLen = commonPrefix(prevKey->getRow(), key->getRow());
    if (rowCommonPrefixLen == -1) {
      fieldsSame |= ROW_SAME;
    } else if (rowCommonPrefixLen > 1) {
      fieldsPrefixed |= ROW_PREFIX;
    }

    cfCommonPrefixLen = commonPrefix(prevKey->getColFamily(), key->getColFamily());

    if (cfCommonPrefixLen == -1) {
      fieldsSame |= CF_SAME;
    } else if (cfCommonPrefixLen > 1) {
      fieldsPrefixed |= CF_PREFIX;
    }

    cqCommonPrefixLen = commonPrefix(prevKey->getColQualifier(), key->getColQualifier());

    if (cqCommonPrefixLen == -1) {
      fieldsSame |= CQ_SAME;
    } else if (cqCommonPrefixLen > 1) {
      fieldsPrefixed |= CQ_PREFIX;
    }

    cvCommonPrefixLen = commonPrefix(prevKey->getColVisibility(), key->getColVisibility());

    if (cvCommonPrefixLen == -1) {
      fieldsSame |= CV_SAME;
    } else if (cvCommonPrefixLen > 1) {
      fieldsPrefixed |= CV_PREFIX;
    }

    tsDiff = key->getTimeStamp() - prevKey->getTimeStamp();

    if (tsDiff == 0) {
      fieldsSame |= TS_SAME;
    } else
      fieldsPrefixed |= TS_DIFF;

    fieldsSame |= fieldsPrefixed == 0 ? 0 : PREFIX_COMPRESSION_ENABLED;

  } else {

  }

  if (key->isDeleted())
    fieldsSame |= DELETED;

}

std::shared_ptr<streams::StreamInterface> RelativeKey::getStream() {
  return key;
}

void RelativeKey::setBase(const std::shared_ptr<Key> &my_key) {
  if (my_key != NULL) {
    key = my_key;
  }
}

void RelativeKey::setPrevious(const std::shared_ptr<Key> &previous_key) {
  if (previous_key != NULL) {

    prevKey = previous_key;
  }
}

bool RelativeKey::readRow(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                          const std::shared_ptr<Key> &newkey) {
  // only use this optimization iff same field.
  if (SH_LIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {
    if (prevText->empty()) {
      auto prevField = prevKey->getRow();
      // this means that we do not have an interned string...likely the first relative key
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }

    newkey->setRow(prevText);
    return true;

  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevKey->getRow();
    auto max = prevKey->getRowSize();
    std::pair<char*, size_t> field;
    bool disown = false;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }

    //if (!prevText->empty() && newkey->getRowStr() != prevText->toString()) {
    //  prevText.reset(new Text(allocatorInstance));
    // }

    /**
     * we need to maintain the previous
     * */
    if (!prevText->empty()) {
      prevText.reset(new Text(allocatorInstance));

    }

    newkey->setRow(field.first, field.second, maxsize, true);

    return true;
  } else {
    auto prev = prevKey->getRow();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);

    newkey->setRow(ret.first, prev.second, ret.second, true);
    return true;
  }

  return false;
}

bool RelativeKey::readCf(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                         const std::shared_ptr<Key> &newkey) {

  // only use this optimization iff same field.
  if (SH_LIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {

    if (prevText->empty()) {
      auto prevField = prevKey->getColFamily();
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }
    newkey->setColumnFamily(prevText);
    return true;

  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevKey->getColFamily();
    auto max = prevKey->getColumnFamilySize();
    std::pair<char*, size_t> field;
    bool disown = false;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }
    /**
     * we need to maintain the previous
     * */
    if (!prevText->empty()) {
      prevText.reset(new Text(allocatorInstance));
    }

    newkey->setColFamily(field.first, field.second, maxsize, true);
    return true;
  } else {
    auto prev = prevKey->getColFamily();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);

    newkey->setColFamily(ret.first, prev.second, ret.second, true);
    return true;
  }

  return false;
}

bool RelativeKey::readCq(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                         const std::shared_ptr<Key> &newkey) {

  if (SH_UNLIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {
    if (prevText->empty()) {
      auto prevField = prevKey->getColQualifier();
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }
    newkey->setColumnQualifier(prevText);
    return true;

  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevKey->getColQualifier();
    auto max = prevKey->getColQualifierSize();
    std::pair<char*, size_t> field;
    bool disown = false;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }
    /**
     * we need to maintain the previous
     * */
    if (!prevText->empty()) {
      prevText.reset(new Text(allocatorInstance));
    }

    newkey->setColQualifier(field.first, field.second, maxsize, true);

    return true;
  } else {
    auto prev = prevKey->getColQualifier();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);


    newkey->setColQualifier(ret.first, prev.second, ret.second, true);
    return true;
  }

  return false;
}

bool RelativeKey::readCv(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed, std::shared_ptr<Text> &prevText,
                         const std::shared_ptr<Key> &newkey) {
  if (SH_LIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {
    if (prevText->empty()) {
      auto prevField = prevKey->getColVisibility();
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }
    newkey->setColumnVisibility(prevText);
    return true;
  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevKey->getColVisibility();
    auto max = prevKey->getColumnVisibilitySize();
    bool disown = false;
    std::pair<char*, size_t> field;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }


    if (!prevText->empty()) {
      prevText.reset(new Text(allocatorInstance));
    }
    newkey->setColVisibility(field.first, field.second, maxsize, true);

    return true;
  } else {
    auto prev = prevKey->getColVisibility();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);
    newkey->setColVisibility(ret.first, prev.second, ret.second, true);

    return true;
  }

  return false;
}

/**
 * Filtered variants
 *
 *
 *
 *
 *
 */
bool RelativeKey::readRowFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed,
                                  std::shared_ptr<Text> &prevText, const std::shared_ptr<Key> &newkey) {
  // only use this optimization iff same field.
  if (SH_LIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {

    if (prevText->empty()) {
      auto prevField = prevKey->getRow();
      // this means that we do not have an interned string...likely the first relative key
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }
    if (likelyFiltered)
          return true;

    newkey->setRow(prevText);
    return true;

  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevFiltered && !prevText->empty() ? prevText->getBuffer() : prevKey->getRow();
    auto max = prevFiltered && !prevText->empty() ? prevText->getMaxSize() : prevKey->getRowSize();
    std::pair<char*, size_t> field;
    bool disown = false;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }

    //if (!prevText->empty() && newkey->getRowStr() != prevText->toString()) {
    //  prevText.reset(new Text(allocatorInstance));
    // }

    /**
     * we need to maintain the previous
     * */
    if (!prevText->empty() || likelyFiltered) {

      if (likelyFiltered) {
        prevText.reset(new Text(allocatorInstance, field.first, field.second, maxsize));
        return true;
      } else {
        prevText.reset(new Text(allocatorInstance));
      }
    }

    newkey->setRow(field.first, field.second, maxsize, true);

    return true;
  } else {
    auto prev = prevFiltered && !prevText->empty() ? prevText->getBuffer() : prevKey->getRow();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);

    if (likelyFiltered) {
      prevText.reset(new Text(allocatorInstance, ret.first, ret.second, maxsize));
      return true;
    }

    newkey->setRow(ret.first, prev.second, ret.second, true);
    return true;
  }

  return false;
}

bool RelativeKey::readCfFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed,
                                 std::shared_ptr<Text> &prevText, const std::shared_ptr<Key> &newkey) {

  // only use this optimization iff same field.
  if (SH_LIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {

    if (prevText->empty()) {
      auto prevField = prevKey->getColFamily();
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }
    if (likelyFiltered)
      return true;
    newkey->setColumnFamily(prevText);
    return true;

  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevFiltered && !prevText->empty() ? prevText->getBuffer() : prevKey->getColFamily();
    auto max = prevFiltered && !prevText->empty() ? prevText->getMaxSize() : prevKey->getColumnFamilySize();
    std::pair<char*, size_t> field;
    bool disown = false;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }
    /**
     * we need to maintain the previous
     * */
    if (!prevText->empty() || likelyFiltered) {
      if (likelyFiltered) {
        prevText.reset(new Text(allocatorInstance, field.first, field.second, maxsize));
        return true;
      } else {
        prevText.reset(new Text(allocatorInstance));
      }
    }

    newkey->setColFamily(field.first, field.second, maxsize, true);
    return true;
  } else {
    auto prev = prevFiltered && !prevText->empty() ? prevText->getBuffer() : prevKey->getColFamily();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);

    if (likelyFiltered) {
      prevText.reset(new Text(allocatorInstance, ret.first, ret.second, maxsize));
      return true;
    }

    newkey->setColFamily(ret.first, prev.second, ret.second, true);
    return true;
  }

  return false;
}

bool RelativeKey::readCqFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed,
                                 std::shared_ptr<Text> &prevText, const std::shared_ptr<Key> &newkey) {

  if (SH_UNLIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {
    if (prevText->empty()) {
      auto prevField = prevKey->getColQualifier();
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }
    if (likelyFiltered)
      return true;
    newkey->setColumnQualifier(prevText);
    return true;

  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevFiltered && !prevText->empty() ? prevText->getBuffer() : prevKey->getColQualifier();
    auto max = prevFiltered && !prevText->empty() ? prevText->getMaxSize() : prevKey->getColQualifierSize();
    std::pair<char*, size_t> field;
    bool disown = false;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }
    /**
     * we need to maintain the previous
     * */
    if (!prevText->empty() || likelyFiltered) {
      if (likelyFiltered) {
        prevText.reset(new Text(allocatorInstance, field.first, field.second, maxsize));
        return true;
      } else {
        prevText.reset(new Text(allocatorInstance));
      }
    }

    newkey->setColQualifier(field.first, field.second, maxsize, true);

    return true;
  } else {
    auto prev = prevFiltered && !prevText->empty() ? prevText->getBuffer() : prevKey->getColQualifier();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);

    if (likelyFiltered) {
      prevText.reset(new Text(allocatorInstance, ret.first, ret.second, maxsize));
      return true;
    }

    newkey->setColQualifier(ret.first, prev.second, ret.second, true);
    return true;
  }

  return false;
}

bool RelativeKey::readCvFiltered(cclient::data::streams::InputStream *stream, int *comparison, uint8_t SAME_FIELD, uint8_t PREFIX, char fieldsSame, char fieldsPrefixed,
                                 std::shared_ptr<Text> &prevText, const std::shared_ptr<Key> &newkey) {
  if (SH_LIKELY((fieldsSame & SAME_FIELD) == SAME_FIELD)) {
    if (likelyFiltered) {
      filtered = true;
      return true;
    }
    if (prevText->empty()) {
      auto prevField = prevKey->getColVisibility();
      auto ret = allocatorInstance->allocateBuffer(prevField.second + 1);

      memcpy_fast(ret.first, prevField.first, prevField.second);

      prevText->reset(ret.first, prevField.second, ret.second);
    }
    if (prevFiltered) {
      filtered = true;
    }
    newkey->setColumnVisibility(prevText);
    return true;
  }

  int maxsize = 0;
  if ((fieldsSame & SAME_FIELD) != SAME_FIELD) {
    auto prevField = prevFiltered && !prevText->empty() ? prevText->getBuffer() : prevKey->getColVisibility();
    auto max = prevFiltered && !prevText->empty() ? prevText->getMaxSize() : prevKey->getColumnVisibilitySize();
    bool disown = false;
    std::pair<char*, size_t> field;
    if ((fieldsPrefixed & PREFIX) == PREFIX) {
      maxsize = readPrefix(stream, &field, &prevField, max, disown);
    } else {
      maxsize = read(stream, &field);
    }

    if (!columnVisibility.empty()) {
      if (!evaluator->evaluate(std::string(field.first, field.second))) {
        filtered = true;
        prevFiltered = true;
        prevText.reset(new Text(allocatorInstance, field.first, field.second, maxsize));
        return true;
      } else {
        prevText.reset(new Text(allocatorInstance));
        prevFiltered = false;
      }
    }

    if (!prevText->empty()) {
      prevText.reset(new Text(allocatorInstance));
    }
    newkey->setColVisibility(field.first, field.second, maxsize, true);

    return true;
  } else {
    if (!columnVisibility.empty()) {
      if (!evaluator->evaluate(prevKey->getColVisibilityStr())) {
        filtered = true;
        prevFiltered = true;
        return true;
      } else {
        prevFiltered = false;
      }
    }

    auto prev = prevKey->getColVisibility();
    auto ret = allocatorInstance->allocateBuffer(prev.second + 1);

    memcpy_fast(ret.first, prev.first, prev.second);
    newkey->setColVisibility(ret.first, prev.second, ret.second, true);

    return true;
  }

  return false;
}

int RelativeKey::readPrefix(streams::InputStream *stream, std::pair<char*, size_t> *row, std::pair<char*, size_t> *prevRow, const size_t &prevsize, bool &disown) {
  uint32_t prefixLen = stream->readHadoopLong();
  uint32_t remainingLen = stream->readHadoopLong();

  int ret = 0;
  auto bfr = allocatorInstance->allocateBuffer(prefixLen + remainingLen + 1);

  row->first = bfr.first;
  ret = bfr.second;
  memcpy_fast(row->first, prevRow->first, prefixLen);
  row->second = prefixLen + remainingLen;

  stream->readBytes(row->first + prefixLen, remainingLen);
  return ret;
}

int RelativeKey::read(cclient::data::streams::InputStream *stream, std::pair<char*, size_t> *row) {
  uint32_t len = stream->readEncodedLong();
  return read(stream, row, len);
}

int RelativeKey::read(cclient::data::streams::InputStream *stream, std::pair<char*, size_t> *input, uint32_t len) {

  auto bfr = allocatorInstance->allocateBuffer(len);
  input->first = bfr.first;
  auto ret = bfr.second;

  input->second = len;
  stream->readBytes(input->first, len);

  return ret;
}

uint64_t RelativeKey::write(streams::OutputStream *outStream) {

  outStream->writeByte(fieldsSame);

  if ((fieldsSame & PREFIX_COMPRESSION_ENABLED) == PREFIX_COMPRESSION_ENABLED) {
    outStream->writeByte(fieldsPrefixed);
  }

  std::pair<char*, size_t> p;

  p = key->getRow();
  if ((fieldsSame & ROW_SAME) == ROW_SAME) {
    // do nothing
  } else if ((fieldsPrefixed & ROW_PREFIX) == ROW_PREFIX) {
    writePrefix(outStream, &p, rowCommonPrefixLen);
  } else {
    //outStream->writeVLong(p.second);
    outStream->writeVLong(p.second);
    outStream->writeBytes(p.first, p.second);
  }

  p = key->getColFamily();
  if ((fieldsSame & CF_SAME) == CF_SAME) {
  } else if ((fieldsPrefixed & CF_PREFIX) == CF_PREFIX) {
    writePrefix(outStream, &p, cfCommonPrefixLen);
  } else {
    outStream->writeVLong(p.second);
    outStream->writeBytes(p.first, p.second);
  }

  p = key->getColQualifier();
  if ((fieldsSame & CQ_SAME) == CQ_SAME) {
    // do nothing
  } else if ((fieldsPrefixed & CQ_PREFIX) == CQ_PREFIX) {
    writePrefix(outStream, &p, cqCommonPrefixLen);
  } else {
    outStream->writeVLong(p.second);
    outStream->writeBytes(p.first, p.second);
  }

  p = key->getColVisibility();
  if ((fieldsSame & CV_SAME) == CV_SAME) {
  } else if ((fieldsPrefixed & CV_PREFIX) == CV_PREFIX) {
    writePrefix(outStream, &p, cvCommonPrefixLen);
  } else {
    outStream->writeVLong(p.second);
    outStream->writeBytes(p.first, p.second);
  }

  if ((fieldsSame & TS_SAME) == TS_SAME) {
  } else if ((fieldsPrefixed & TS_DIFF) == TS_DIFF) {
    outStream->writeVLong(tsDiff);
  } else {
    outStream->writeVLong(key->getTimeStamp());
  }

  return outStream->getPos();

}

bool RelativeKey::operator <(const RelativeKey &rhs) const {
  return key < rhs.key;
}

bool RelativeKey::operator <(const RelativeKey *rhs) const {
  return *this < *rhs;

}

RelativeKey::~RelativeKey() {

}

void RelativeKey::setKey(const std::shared_ptr<Key> &keyToCopy, const std::shared_ptr<Key> &keyToCopyTo) {
  std::pair<char*, size_t> p = keyToCopy->getRow();
  keyToCopyTo->setRow(p.first, p.second);

  p = keyToCopy->getColFamily();
  keyToCopyTo->setColFamily(p.first, p.second);
  p = keyToCopy->getColQualifier();
  keyToCopyTo->setColQualifier(p.first, p.second);
  p = keyToCopy->getColVisibility();
  keyToCopyTo->setColVisibility(p.first, p.second);
  keyToCopyTo->setTimeStamp(keyToCopy->getTimeStamp());

}

bool RelativeKey::isSame(std::pair<char*, size_t> a, std::pair<char*, size_t> b) {
  if (a.second > 0 && a.second == b.second) {
    return (memcmp(a.first, b.first, a.second) == 0);
  } else
    return false;
}
}
}
