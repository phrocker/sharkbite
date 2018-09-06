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

#include "data/constructs/rkey.h"

namespace cclient {
namespace data {

RelativeKey::RelativeKey()
    : fieldsSame(0),
      fieldsPrefixed(0),
      key(NULL),
      prevKey(NULL) {

}

int RelativeKey::commonPrefix(std::pair<char*, size_t> prev,
                              std::pair<char*, size_t> curr) {
  if (prev == curr)
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

RelativeKey::RelativeKey(std::shared_ptr<Key> previous_key,
                         std::shared_ptr<Key> my_key) {
  if (my_key == NULL)
    throw std::runtime_error("Key must not be null");

  key = std::make_shared<Key>();
  prevKey = NULL;
  fieldsSame = 0;
  fieldsPrefixed = 0;
  setKey(my_key, key);

  if (previous_key != NULL) {
    prevKey = std::make_shared<Key>();
    setKey(previous_key, prevKey);

    rowCommonPrefixLen = commonPrefix(prevKey->getRow(), key->getRow());
    if (rowCommonPrefixLen == -1) {
      fieldsSame |= ROW_SAME;
    } else if (rowCommonPrefixLen > 1) {
      fieldsPrefixed |= ROW_PREFIX;
    }

    cfCommonPrefixLen = commonPrefix(prevKey->getColFamily(),
                                     key->getColFamily());

    if (cfCommonPrefixLen == -1) {
      fieldsSame |= CF_SAME;
    } else if (cfCommonPrefixLen > 1) {
      fieldsPrefixed |= CF_PREFIX;
    }

    cqCommonPrefixLen = commonPrefix(prevKey->getColQualifier(),
                                     key->getColQualifier());

    if (cqCommonPrefixLen == -1) {
      fieldsSame |= CQ_SAME;
    } else if (cqCommonPrefixLen > 1) {
      fieldsPrefixed |= CQ_PREFIX;
    }

    cvCommonPrefixLen = commonPrefix(prevKey->getColVisibility(),
                                     key->getColVisibility());

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

void RelativeKey::setBase(std::shared_ptr<Key> my_key) {
  if (my_key != NULL) {
    key = std::make_shared<Key>();
    setKey(my_key, key);
  }
}

void RelativeKey::setPrevious(std::shared_ptr<Key> previous_key) {
  if (previous_key != NULL) {

    prevKey = std::make_shared<Key>();
    setKey(previous_key, prevKey);
  }
}

uint64_t RelativeKey::read(streams::InputStream *stream) {
  fieldsSame = stream->readByte();

  if ((fieldsSame & PREFIX_COMPRESSION_ENABLED) == PREFIX_COMPRESSION_ENABLED) {
    fieldsPrefixed = stream->readByte();
  } else {
    fieldsPrefixed = 0;
  }

  std::vector<char> row, cf, cq, cv;
  std::vector<char> prevRow, prevCf, prevCq, prevVis;

  std::pair<char*, size_t> scratch = prevKey->getRow();

  prevRow.insert(prevRow.begin(), scratch.first,
                 scratch.first + scratch.second);
  scratch = prevKey->getColFamily();
  prevCf.insert(prevCf.begin(), scratch.first, scratch.first + scratch.second);
  scratch = prevKey->getColQualifier();
  prevCq.insert(prevCq.begin(), scratch.first, scratch.first + scratch.second);

  scratch = prevKey->getColVisibility();
  prevVis.insert(prevVis.begin(), scratch.first,
                 scratch.first + scratch.second);

  uint64_t timestamp = 0;
  uint64_t prevTimestamp = prevKey->getTimeStamp();

  int rowCmp = -1, cfCmp = -1, cqCmp = -1, cvCmp = -1;

  readPrefix(stream, &rowCmp, RelativeKey::ROW_SAME, RelativeKey::ROW_PREFIX,
             fieldsSame, fieldsPrefixed, &row, &prevRow);

  if (readPrefix(stream, &cfCmp, RelativeKey::CF_SAME, RelativeKey::CF_PREFIX,
                 fieldsSame, fieldsPrefixed, &cf, &prevCf)) {

  }

  if (readPrefix(stream, &cqCmp, RelativeKey::CQ_SAME, RelativeKey::CQ_PREFIX,
                 fieldsSame, fieldsPrefixed, &cq, &prevCq)) {

  }

  if (readPrefix(stream, &cvCmp, RelativeKey::CV_SAME, RelativeKey::CV_PREFIX,
                 fieldsSame, fieldsPrefixed, &cv, &prevVis)) {

  }

  if ((fieldsSame & RelativeKey::TS_SAME) != RelativeKey::TS_SAME) {
    prevTimestamp = timestamp;

    timestamp = stream->readEncodedLong();
    if ((fieldsSame & RelativeKey::TS_DIFF) != RelativeKey::TS_DIFF) {
      timestamp += prevTimestamp;
    }
  } else
    timestamp = prevTimestamp;


  key = std::make_shared<Key>();
  key->setRow(row.data(), row.size());
  key->setColFamily(cf.data(), cf.size());
  key->setColQualifier(cq.data(), cq.size());
  key->setColVisibility(cv.data(), cv.size());
  key->setTimeStamp(timestamp);

  prevKey = key;

  return stream->getPos();

}

void RelativeKey::readPrefix(streams::InputStream *stream,
                             std::vector<char> *row,
                             std::vector<char> *prevRow) {
  uint32_t prefixLen = stream->readHadoopLong();
  uint32_t remainingLen = stream->readHadoopLong();
  row->insert(row->begin(), prevRow->data(), prevRow->data() + prefixLen);
  char *array = new char[remainingLen];
  stream->readBytes(array, remainingLen);
  row->insert(row->end(), array, array + remainingLen);
  delete[] array;
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

void RelativeKey::setKey(std::shared_ptr<Key> keyToCopy,
                         std::shared_ptr<Key> keyToCopyTo) {
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

bool RelativeKey::isSame(std::pair<char*, size_t> a,
                         std::pair<char*, size_t> b) {
  if (a.second > 0 && a.second == b.second) {
    return (memcmp(a.first, b.first, a.second) == 0);
  } else
    return false;
}
}
}
